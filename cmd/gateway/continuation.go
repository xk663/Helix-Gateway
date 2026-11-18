package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"helix-gateway-local/internal/truncationfix"
)

const continuationTimeout = 90 * time.Second
const continuationPartialLimit = 64 << 10
const continuedTrailer = "X-Helix-Continued"

type continuationConfig struct {
	hops   int
	tokens int
}

func configuredContinuation() continuationConfig {
	hops, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("TRUNCATIONFIX_CONTINUE")))
	tokens, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("TRUNCATIONFIX_CONTINUE_TOKENS")))
	if tokens <= 0 {
		tokens = defaultTokenCeiling
	}
	return continuationConfig{hops: min(hops, 8), tokens: tokens}
}

type chatObject map[string]json.RawMessage

type continuation struct {
	config       continuationConfig
	request      chatObject
	messages     []json.RawMessage
	deadline     time.Time
	clientCap    int
	reserved     int
	remaining    int
	hops         int
	wantUsage    bool
	disabled     bool
	tooLong      bool
	partial      strings.Builder
	overlap      truncationfix.TextOverlap
	base         chatObject
	finish       string
	actualLength bool
	finishChunk  chatObject
	usageChunk   json.RawMessage
	hopUsage     *completionUsage
	totals       completionUsage
	usageUnknown bool
	open         func(context.Context, []byte) (io.ReadCloser, error)
	trailer      http.Header
}

type completionUsage struct {
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
	Total      int64 `json:"total_tokens"`
}

func newContinuation(config continuationConfig, original []byte, clientCap, sentCap int) *continuation {
	if config.hops <= 0 || clientCap <= 0 || sentCap <= 0 || sentCap > config.tokens {
		return nil
	}
	var request chatObject
	if json.Unmarshal(original, &request) != nil || !bytes.Equal(request["stream"], []byte("true")) {
		return nil
	}
	if n := request["n"]; len(n) != 0 && string(n) != "1" && string(n) != "null" {
		return nil
	}
	for _, field := range []string{"tools", "functions", "function_call", "tool_choice", "audio", "prediction", "modalities"} {
		if v := string(request[field]); v != "" && v != "null" && v != "[]" && !(field == "tool_choice" && v == `"auto"`) {
			return nil
		}
	}
	if v := string(request["response_format"]); v != "" && v != "null" {
		var format struct{ Type string }
		if json.Unmarshal(request["response_format"], &format) != nil || format.Type != "text" {
			return nil
		}
	}
	var messages []json.RawMessage
	if json.Unmarshal(request["messages"], &messages) != nil || len(messages) == 0 {
		return nil
	}
	var options struct {
		Usage bool `json:"include_usage"`
	}
	_ = json.Unmarshal(request["stream_options"], &options)
	return &continuation{
		config: config, request: request, messages: messages,
		deadline: time.Now().Add(continuationTimeout), clientCap: clientCap,
		reserved: sentCap, remaining: config.tokens, wantUsage: options.Usage,
	}
}

func (c *continuation) chunk(raw []byte, fix *truncationfix.Request, upstreamLength bool) ([]byte, error) {
	if c.disabled {
		return raw, nil
	}
	var root chatObject
	var choices []chatObject
	if json.Unmarshal(raw, &root) != nil || json.Unmarshal(root["choices"], &choices) != nil {
		return nil, truncationfix.ErrInvalidResponse
	}
	if len(choices) == 0 {
		if present(root["usage"]) {
			c.captureUsage(root, raw)
			return nil, nil
		}
		if c.hops == 0 {
			return raw, nil
		}
		return nil, nil
	}
	choice := choices[0]
	var delta chatObject
	if v := choice["delta"]; present(v) && json.Unmarshal(v, &delta) != nil {
		return nil, truncationfix.ErrInvalidResponse
	}
	var text string
	content := delta["content"]
	nonText := present(content) && json.Unmarshal(content, &text) != nil
	if len(choices) != 1 || (present(choice["index"]) && string(choice["index"]) != "0") ||
		present(delta["tool_calls"]) || present(delta["function_call"]) || present(delta["audio"]) ||
		present(delta["refusal"]) || nonText {
		if c.hops > 0 || c.finish != "" {
			return nil, truncationfix.ErrInvalidResponse
		}
		c.disabled = true
		return raw, nil
	}
	if c.finish != "" {
		return nil, truncationfix.ErrInvalidResponse
	}
	hasUsage := present(root["usage"])
	if hasUsage {
		c.captureUsage(root, nil)
		delete(root, "usage")
	}
	if c.base == nil {
		c.base = make(chatObject)
		for _, key := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
			if root[key] != nil {
				c.base[key] = root[key]
			}
		}
	}
	c.finish = textValue(choice["finish_reason"])
	c.actualLength = c.finish == "length" && (upstreamLength || c.hops > 0)
	if c.hops > 0 {
		text, removed := c.overlap.Merge(c.partial.String(), text, c.finish != "")
		c.addText(text)
		fix.Record(truncationfix.Report{RecoveredBytes: uint64(len(text)), DeduplicatedBytes: uint64(removed)})
		if text == "" {
			return nil, nil
		}
		return c.contentChunk(text), nil
	}
	c.addText(text)
	if c.finish == "" {
		if hasUsage {
			return marshalJSON(root), nil
		}
		return raw, nil
	}
	c.finishChunk = maps.Clone(root)
	finish := maps.Clone(choice)
	finish["delta"] = json.RawMessage(`{}`)
	c.finishChunk["choices"] = marshalJSON([]chatObject{finish})
	if len(delta) == 0 {
		return nil, nil
	}
	choice["finish_reason"] = json.RawMessage("null")
	root["choices"] = marshalJSON(choices)
	return marshalJSON(root), nil
}

func (c *continuation) captureUsage(root chatObject, raw []byte) {
	c.hopUsage = readCompletionUsage(root["usage"])
	if c.hops != 0 {
		return
	}
	if len(raw) == 0 {
		usage := maps.Clone(root)
		usage["choices"] = json.RawMessage(`[]`)
		raw = marshalJSON(usage)
	}
	c.usageChunk = raw
}

func (c *continuation) addText(text string) {
	if c.tooLong {
		return
	}
	if len(text) > continuationPartialLimit-c.partial.Len() {
		c.tooLong = true
		return
	}
	c.partial.WriteString(text)
}

func (c *continuation) contentChunk(text string) []byte {
	frame := maps.Clone(c.base)
	frame["choices"] = marshalJSON([]any{map[string]any{
		"index": 0, "delta": map[string]string{"content": text}, "finish_reason": nil,
	}})
	return marshalJSON(frame)
}

func (c *continuation) next(ctx context.Context, fix *truncationfix.Request) io.ReadCloser {
	c.accountHop()
	if c.disabled || c.tooLong || !c.actualLength || c.partial.Len() == 0 || c.hops >= c.config.hops {
		return nil
	}
	if c.remaining < c.clientCap || time.Now().After(c.deadline) {
		fix.Note("continuation_budget")
		return nil
	}
	partial := marshalJSON(map[string]string{"role": "assistant", "content": c.partial.String()})
	prompt := marshalJSON(map[string]string{"role": "user", "content": "Continue exactly where you stopped. No preamble, no repetition."})
	next := maps.Clone(c.request)
	messages := make([]json.RawMessage, 0, len(c.messages)+2)
	messages = append(messages, c.messages...)
	next["messages"] = marshalJSON(append(messages, partial, prompt))
	payload := marshalJSON(next)
	if len(payload) > maxBody {
		fix.Note("continuation_budget")
		return nil
	}
	hopCtx, cancel := context.WithDeadline(ctx, c.deadline)
	body, err := c.open(hopCtx, payload)
	if err != nil {
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			fix.Note("continuation_budget")
		} else {
			fix.Note("continuation_upstream")
		}
		return nil
	}
	c.hops++
	c.reserved = c.clientCap
	c.finish, c.finishChunk, c.usageChunk, c.hopUsage = "", nil, nil, nil
	c.actualLength = false
	c.overlap = truncationfix.TextOverlap{}
	c.trailer.Set(continuedTrailer, strconv.Itoa(c.hops))
	return continuationBody{ReadCloser: body, cancel: cancel}
}

func (c *continuation) accountHop() {
	spent := c.reserved
	if c.hopUsage != nil {
		if c.hopUsage.Completion <= math.MaxInt {
			spent = int(c.hopUsage.Completion)
		}
		c.totals.add(*c.hopUsage)
	} else {
		c.usageUnknown = true
	}
	c.remaining -= min(c.remaining, spent)
}

func (c *continuation) end(terminal []byte, failed bool, fix *truncationfix.Request, report *truncationfix.Report) []byte {
	var result bytes.Buffer
	if c.disabled {
		return terminal
	}
	if c.hops > 0 && c.finish == "" {
		text, removed := c.overlap.Merge(c.partial.String(), "", true)
		c.addText(text)
		report.RecoveredBytes += uint64(len(text))
		report.DeduplicatedBytes += uint64(removed)
		if text != "" {
			writeSSEData(&result, c.contentChunk(text))
		}
	}
	if !failed && !c.tooLong && c.partial.Len() != 0 {
		suffix, repair := fix.RepairText(c.partial.String())
		report.Repairs += repair.Repairs
		report.GuardTrips += repair.GuardTrips
		if len(repair.Reasons) != 0 {
			if report.Reasons == nil {
				report.Reasons = make(map[string]uint64)
			}
			for reason, count := range repair.Reasons {
				report.Reasons[reason] += count
			}
		}
		if suffix != "" {
			writeSSEData(&result, c.contentChunk(suffix))
		}
	}
	if len(terminal) != 0 && (c.hops == 0 || failed) {
		writeSSEData(&result, terminal)
	} else if c.hops > 0 {
		reason := c.finish
		if reason == "" {
			reason = "length"
		}
		metadata := maps.Clone(c.base)
		metadata["choices"] = marshalJSON([]any{map[string]any{
			"index": 0, "delta": map[string]string{}, "finish_reason": reason,
		}})
		writeSSEData(&result, marshalJSON(metadata))
	} else if c.finishChunk != nil {
		writeSSEData(&result, marshalJSON(c.finishChunk))
	}
	if c.hops == 0 && len(c.usageChunk) != 0 {
		writeSSEData(&result, c.usageChunk)
	} else if c.hops > 0 && c.wantUsage && !c.usageUnknown && !failed {
		usage := maps.Clone(c.base)
		usage["choices"] = json.RawMessage(`[]`)
		usage["usage"] = marshalJSON(c.totals)
		writeSSEData(&result, marshalJSON(usage))
	}
	return result.Bytes()
}

func present(raw json.RawMessage) bool { return len(raw) != 0 && string(raw) != "null" }

func textValue(raw json.RawMessage) string {
	var text string
	_ = json.Unmarshal(raw, &text)
	return text
}

func marshalJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func readCompletionUsage(raw json.RawMessage) *completionUsage {
	var reported struct {
		Prompt     *int64 `json:"prompt_tokens"`
		Completion *int64 `json:"completion_tokens"`
	}
	if json.Unmarshal(raw, &reported) != nil || reported.Prompt == nil || reported.Completion == nil ||
		*reported.Prompt < 0 || *reported.Completion < 0 || *reported.Prompt > math.MaxInt64-*reported.Completion {
		return nil
	}
	return &completionUsage{Prompt: *reported.Prompt, Completion: *reported.Completion, Total: *reported.Prompt + *reported.Completion}
}

func (u *completionUsage) add(other completionUsage) {
	u.Prompt += min(other.Prompt, math.MaxInt64-u.Prompt)
	u.Completion += min(other.Completion, math.MaxInt64-u.Completion)
	u.Total += min(other.Total, math.MaxInt64-u.Total)
}

type continuationBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b continuationBody) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}

func (c *continuation) bind(resp *http.Response, transport http.RoundTripper, state *chatState) {
	template := resp.Request
	c.trailer = make(http.Header)
	c.trailer.Set(continuedTrailer, "0")
	resp.Trailer = c.trailer
	c.open = func(ctx context.Context, raw []byte) (io.ReadCloser, error) {
		req := template.Clone(ctx)
		req.Body = io.NopCloser(bytes.NewReader(raw))
		req.GetBody = nil
		req.ContentLength = int64(len(raw))
		req.TransferEncoding = nil
		req.Header.Del("Content-Length")
		req.Header.Set("Accept-Encoding", "identity")
		state.upstreamCalls++
		next, err := transport.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		media, _, _ := mime.ParseMediaType(next.Header.Get("Content-Type"))
		if next.StatusCode != http.StatusOK || media != "text/event-stream" {
			_ = next.Body.Close()
			return nil, errors.New("continuation upstream did not return an SSE stream")
		}
		body, reason := decodeBody(next)
		if reason != "" {
			_ = next.Body.Close()
			return nil, errors.New(reason)
		}
		state.continuationHops++
		return body, nil
	}
}
