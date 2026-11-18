package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"helix-gateway-local/internal/truncationfix"
)

type responseFailure struct{ message string }

func (e responseFailure) Error() string { return e.message }

func reject(fix *truncationfix.Request, reason, message string) error {
	fix.Guard(reason)
	return responseFailure{message: message}
}

func rewriteResponse(resp *http.Response, transport http.RoundTripper) error {
	fix := requestFix(resp.Request)
	if fix == nil {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		if !fix.Synthetic() {
			fix.Note("upstream_status")
			return nil
		}
		return reject(fix, "upstream_status", fmt.Sprintf("Upstream rejected the tool-wrapped request (HTTP %d). The response was withheld.", resp.StatusCode))
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (media != "text/event-stream" && media != "application/json") {
		if !fix.Synthetic() {
			fix.Note("upstream_content_type")
			return nil
		}
		return reject(fix, "upstream_content_type", "Unsupported upstream Content-Type for a tool-wrapped response.")
	}

	decoded, reason := decodeBody(resp)
	switch reason {
	case "upstream_gzip":
		return reject(fix, reason, "Invalid upstream gzip response.")
	case "upstream_encoding":
		return reject(fix, "upstream_encoding", "Unsupported upstream Content-Encoding.")
	}
	resp.Body = decoded
	stripTransformedHeaders(resp, fix.Synthetic())
	if media == "text/event-stream" {
		body := newSSEBody(resp.Body, fix)
		body.ctx = resp.Request.Context()
		body.delivery = requestState(resp.Request)
		if body.delivery != nil && body.delivery.continuation != nil {
			body.continuation = body.delivery.continuation
			body.continuation.bind(resp, transport, body.delivery)
			body.stream.DeferRepair(true)
		}
		resp.Body = body
		resp.ContentLength = -1
		return nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return reject(fix, "upstream_read", "The upstream JSON response could not be read.")
	}
	if len(raw) > maxBody {
		return reject(fix, "upstream_body_limit", "The upstream JSON response exceeds the gateway limit.")
	}
	result, err := fix.Complete(raw)
	if err != nil {
		return reject(fix, "upstream_protocol", "Unexpected upstream JSON response. The payload was withheld.")
	}
	if state := requestState(resp.Request); state != nil {
		state.completed = true
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(result))
	resp.ContentLength = int64(len(result))
	resp.Header.Set("Content-Length", strconv.Itoa(len(result)))
	return nil
}

func decodeBody(resp *http.Response) (io.ReadCloser, string) {
	switch strings.ToLower(strings.TrimSpace(strings.Join(resp.Header.Values("Content-Encoding"), ","))) {
	case "", "identity":
		return resp.Body, ""
	case "gzip":
		reader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, "upstream_gzip"
		}
		return &gzipBody{Reader: reader, upstream: resp.Body}, ""
	default:
		return nil, "upstream_encoding"
	}
}

type gzipBody struct {
	*gzip.Reader
	upstream io.Closer
}

func (body *gzipBody) Close() error {
	return errors.Join(body.Reader.Close(), body.upstream.Close())
}

func stripTransformedHeaders(resp *http.Response, synthetic bool) {
	for _, name := range []string{"Content-Encoding", "Content-Length", "Transfer-Encoding", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "ETag", "Trailer"} {
		resp.Header.Del(name)
	}
	resp.Trailer = nil
	if !synthetic {
		return
	}
	for key, values := range resp.Header {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "access-control-") || strings.Contains(lower, truncationfix.ToolName) {
			resp.Header.Del(key)
			continue
		}
		for _, value := range values {
			if strings.Contains(value, truncationfix.ToolName) {
				resp.Header.Del(key)
				break
			}
		}
	}
}

var errEventLimit = errors.New("upstream SSE event exceeds gateway limit")

type sseBody struct {
	io.ReadCloser
	reader       *bufio.Reader
	fix          *truncationfix.Request
	stream       *truncationfix.Stream
	ctx          context.Context
	delivery     *chatState
	pending      []byte
	upstreamErr  error
	seenChunk    bool
	continuation *continuation
	done         bool
	completed    bool
	terminal     truncationfix.Report
}

func newSSEBody(upstream io.ReadCloser, fix *truncationfix.Request) *sseBody {
	return &sseBody{
		ReadCloser: upstream, reader: bufio.NewReaderSize(upstream, fix.BufferSize()),
		fix: fix, stream: fix.Stream(), ctx: context.Background(),
	}
}

func (body *sseBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(body.pending) == 0 && !body.done {
		if body.ctx.Err() != nil {
			return 0, io.EOF
		}
		body.pending = body.next()
	}
	if len(body.pending) == 0 || body.ctx.Err() != nil {
		return 0, io.EOF
	}
	n := copy(p, body.pending)
	body.pending = body.pending[n:]
	if body.done && len(body.pending) == 0 && body.delivery != nil {
		body.delivery.terminal = &body.terminal
		body.delivery.completed = body.completed
	}
	return n, nil
}

func (body *sseBody) next() []byte {
	if body.upstreamErr != nil {
		switch {
		case body.upstreamErr == io.EOF && body.seenChunk:
			return body.finishHop(true)
		case errors.Is(body.upstreamErr, errEventLimit):
			return body.terminate("sse_event_limit", false)
		case body.upstreamErr == io.EOF:
			return body.terminate("empty_stream", false)
		default:
			return body.terminate("upstream_stream_read", false)
		}
	}
	wire, err := readFrame(body.reader)
	body.upstreamErr = err
	if len(wire) == 0 {
		return nil
	}

	event, isChunk, done, err := body.rewriteEvent(wire)
	if err != nil {
		return body.terminate("upstream_stream_protocol", false)
	}
	body.seenChunk = body.seenChunk || isChunk
	if done {
		if !body.seenChunk {
			return body.terminate("empty_stream", false)
		}
		return body.finishHop(false)
	}
	if len(event) > 0 && body.upstreamErr != nil && !bytes.HasSuffix(event, []byte("\n\n")) && !bytes.HasSuffix(event, []byte("\n\r\n")) {
		event = append(event, '\n', '\n')
	}
	return event
}

func (body *sseBody) finishHop(missingDone bool) []byte {
	if body.continuation != nil && body.ctx.Err() == nil {
		if next := body.continuation.next(body.ctx, body.fix); next != nil {
			if missingDone {
				body.fix.Record(truncationfix.Report{MissingDone: 1})
			}
			_ = body.ReadCloser.Close()
			body.ReadCloser = next
			body.reader.Reset(next)
			body.stream = body.fix.RepairStream()
			body.stream.DeferRepair(true)
			body.seenChunk, body.upstreamErr = false, nil
			return nil
		}
	}
	return body.terminate("", missingDone)
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	var wire bytes.Buffer
	lineStart := 0
	for {
		part, err := reader.ReadSlice('\n')
		if wire.Len()+len(part) > maxEvent {
			return nil, errEventLimit
		}
		wire.Write(part)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			return wire.Bytes(), io.EOF
		}
		line := wire.Bytes()[lineStart:]
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			return wire.Bytes(), nil
		}
		lineStart = wire.Len()
	}
}

func (body *sseBody) rewriteEvent(wire []byte) ([]byte, bool, bool, error) {
	var payload, fields bytes.Buffer
	haveData := false
	for _, line := range bytes.SplitAfter(wire, []byte("\n")) {
		value := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if bytes.ContainsRune(value, '\r') {
			return nil, false, false, truncationfix.ErrInvalidResponse
		}
		field, data, _ := bytes.Cut(value, []byte(":"))
		if bytes.Equal(field, []byte("data")) {
			if haveData {
				payload.WriteByte('\n')
			}
			haveData = true
			payload.Write(bytes.TrimPrefix(data, []byte(" ")))
		} else if len(value) > 0 {
			if body.fix.Synthetic() && bytes.Contains(value, []byte(truncationfix.ToolName)) {
				return nil, false, false, truncationfix.ErrInvalidResponse
			}
			fields.Write(line)
		}
	}
	if !haveData {
		return wire, false, false, nil
	}
	raw := payload.Bytes()
	if bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
		return nil, false, true, nil
	}
	priorLength := body.fix.Report().UpstreamLength
	chunk, err := body.stream.Chunk(raw)
	if err != nil {
		return nil, false, false, err
	}
	if body.continuation != nil && len(chunk) != 0 {
		chunk, err = body.continuation.chunk(chunk, body.fix, body.fix.Report().UpstreamLength > priorLength)
		if err != nil {
			return nil, false, false, err
		}
		if body.continuation.disabled {
			body.stream.DeferRepair(false)
		}
	}
	if len(chunk) == 0 {
		// Preserve comment heartbeats without forwarding an empty delta.
		if fields.Len() > 0 {
			fields.WriteByte('\n')
		}
		return fields.Bytes(), true, false, nil
	}
	if bytes.Equal(raw, chunk) {
		return wire, true, false, nil
	}
	if fields.Len() > 0 && !bytes.HasSuffix(fields.Bytes(), []byte("\n")) {
		fields.WriteByte('\n')
	}
	fields.WriteString("data: ")
	fields.Write(chunk)
	fields.WriteString("\n\n")
	return fields.Bytes(), true, false, nil
}

func (body *sseBody) terminate(reason string, missingDone bool) []byte {
	body.done = true
	if body.ctx.Err() != nil {
		return nil
	}
	var buffer bytes.Buffer
	terminal, report := body.stream.End(reason != "")
	if len(terminal) > 0 && body.fix.CheckOutput(terminal) != nil {
		reason = "upstream_stream_protocol"
		terminal, report = body.stream.End(true)
		if body.fix.CheckOutput(terminal) != nil {
			terminal = []byte(`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`)
			report = truncationfix.Report{GatewayLength: 1}
		}
	}
	if reason != "" {
		switch reason {
		case "sse_event_limit", "upstream_stream_protocol":
			body.fix.Guard(reason)
		default:
			body.fix.Note(reason)
		}
		buffer.WriteString("data: {\"error\":{\"message\":\"The upstream SSE response ended unexpectedly or was malformed.\",\"code\":\"upstream_stream_error\"}}\n\n")
	}
	if body.continuation != nil && !body.continuation.disabled {
		buffer.Write(body.continuation.end(terminal, reason != "", body.fix, &report))
	} else {
		writeSSEData(&buffer, terminal)
	}
	buffer.WriteString("data: [DONE]\n\n")
	if missingDone && reason == "" {
		report.MissingDone++
	}
	body.terminal = report
	body.completed = reason == "" && report.GatewayLength == 0
	return buffer.Bytes()
}

func writeSSEData(buffer *bytes.Buffer, payload []byte) {
	if len(payload) != 0 {
		buffer.WriteString("data: ")
		buffer.Write(payload)
		buffer.WriteString("\n\n")
	}
}
