package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"helix-gateway-local/internal/truncationfix"
)

const chatRequest = `{"model":"qwen-mock","messages":[{"role":"user","content":"hello"}]}`

func sseFixture() []byte {
	return fmt.Appendf(nil, ": padding-123456\r\n\r\ndata: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":%q,\"arguments\":\"{\\\"content\\\":\\\"雪山\"}}]}}]}\r\n\r\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\r\n\r\ndata: [DONE]\r\n\r\n", truncationfix.ToolName)
}

func TestUpstreamResponseBranches(t *testing.T) {
	name := truncationfix.ToolName
	stream := sseFixture()
	complete := fmt.Appendf(nil, `{"choices":[{"index":0,"message":{"tool_calls":[{"function":{"name":%q,"arguments":"{\"content\":\"你好\"}"}}]},"finish_reason":"tool_calls"}]}`, name)
	for _, tc := range []struct {
		label, media, encoding, reason string
		status, wantStatus             int
		body                           []byte
		wantText                       string
		recovered, lengths             uint64
	}{
		{label: "gzip JSON", media: "application/json; charset=utf-8", encoding: "gzip", status: 200, wantStatus: 200, body: complete, wantText: "你好", recovered: 6},
		{label: "gzip SSE", media: "text/event-stream", encoding: "gzip", status: 200, wantStatus: 200, body: stream, wantText: "雪山", recovered: 6, lengths: 1, reason: "incomplete_tool_arguments"},
		{label: "invalid gzip", media: "application/json", encoding: "broken-gzip", status: 200, wantStatus: 502, body: complete, reason: "upstream_gzip"},
		{label: "unsupported encoding", media: "application/json", encoding: "br", status: 200, wantStatus: 502, body: complete, reason: "upstream_encoding"},
		{label: "4xx reflection", media: "application/json", status: 429, wantStatus: 502, body: complete, reason: "upstream_status"},
		{label: "wrong content type", media: "text/html", status: 200, wantStatus: 502, body: complete, reason: "upstream_content_type"},
		{label: "JSON error reflection", media: "application/json", status: 200, wantStatus: 502, body: fmt.Appendf(nil, `{"error":{"message":%q}}`, name), reason: "upstream_protocol"},
		{label: "legacy function call reflection", media: "application/json", status: 200, wantStatus: 502,
			body:   []byte(`{"choices":[{"message":{"function_call":{"name":"\u0061gw_emit_response","arguments":"{}"}},"finish_reason":"function_call"}]}`),
			reason: "upstream_protocol"},
		{label: "malformed stream", media: "text/event-stream", status: 200, wantStatus: 200, body: []byte("data: {\"oops\":\"" + name + "\"\r\n\r\n"), wantText: "upstream_stream_error", reason: "upstream_stream_protocol", lengths: 1},
	} {
		t.Run(tc.label, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("unexpected upstream Accept-Encoding")
				}
				w.Header().Set("Content-Type", tc.media)
				w.Header().Set("X-Reflected-Tool", name)
				if tc.encoding == "broken-gzip" {
					w.Header().Set("Content-Encoding", "gzip")
				} else if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.WriteHeader(tc.status)
				if tc.encoding == "gzip" {
					writer := gzip.NewWriter(w)
					_, _ = writer.Write(tc.body)
					_ = writer.Close()
				} else {
					_, _ = w.Write(tc.body)
				}
			}))
			defer up.Close()
			target, _ := url.Parse(up.URL + "/v1")
			gateway := newGateway(target, (truncationfix.Config{Enabled: true}).Resolve())
			response := httptest.NewRecorder()
			gateway.ServeHTTP(response, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(chatRequest)))
			if response.Code != tc.wantStatus || bytes.Contains(response.Body.Bytes(), []byte(name)) ||
				response.Header().Get("X-Reflected-Tool") != "" || response.Header().Get("Content-Encoding") != "" {
				t.Fatalf("HTTP %d headers=%v response=%q", response.Code, response.Header(), response.Body)
			}
			if !bytes.Contains(response.Body.Bytes(), []byte(tc.wantText)) ||
				(tc.media == "text/event-stream" && !bytes.Contains(response.Body.Bytes(), []byte("data: [DONE]"))) {
				t.Fatalf("expected %q and clean termination: %s", tc.wantText, response.Body)
			}
			stats := httptest.NewRecorder()
			gateway.ServeHTTP(stats, httptest.NewRequest("GET", "/stats", nil))
			var snapshot statsSnapshot
			if err := json.Unmarshal(stats.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.Interventions != 1 || snapshot.SyntheticRequests != 1 ||
				snapshot.RecoveredBytes != tc.recovered || snapshot.LengthDecisions != tc.lengths ||
				(tc.reason != "" && snapshot.FallbackReasons[tc.reason] != 1) {
				t.Fatalf("unexpected counters: %s", stats.Body)
			}
		})
	}
}

type fragmentReader struct {
	data   []byte
	sizes  []byte
	offset int
}

func (r *fragmentReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	size := 1
	if len(r.sizes) > 0 {
		size += int(r.sizes[r.offset%len(r.sizes)])
		r.offset++
	}
	n := copy(p, r.data[:min(size, len(r.data))])
	r.data = r.data[n:]
	return n, nil
}

func parseFixture(t testing.TB, reader io.Reader) []byte {
	t.Helper()
	_, req, _ := (truncationfix.Config{Enabled: true, StreamBufferSize: 16}).Resolve().Prepare([]byte(chatRequest))
	if req == nil {
		t.Fatal("fixture was not wrapped")
	}
	body := newSSEBody(io.NopCloser(reader), req)
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestFragmentedCRLFAndUTF8(t *testing.T) {
	data := sseFixture()
	for _, sizes := range [][]byte{nil, {14, 0, 0, 2, 15, 3}, {255, 0, 0}} {
		out := parseFixture(t, &fragmentReader{data: data, sizes: sizes})
		if !bytes.Contains(out, []byte("雪山")) || !bytes.Contains(out, []byte("\"finish_reason\":\"length\"")) ||
			!bytes.HasSuffix(out, []byte("data: [DONE]\n\n")) || bytes.Contains(out, []byte(truncationfix.ToolName)) {
			t.Fatalf("malformed output under fragmentation %v: %q", sizes, out)
		}
	}
}

func FuzzChunk(f *testing.F) {
	f.Add(sseFixture(), []byte{14, 0, 2, 0, 1, 15})
	f.Add(append([]byte("data: {\"choices\":[]}\r\n\r\n: "), []byte(truncationfix.ToolName)...), []byte{0, 1})
	f.Add([]byte("data: {\"error\":{\"message\":\"no\"}}\n\n"), []byte{})
	f.Fuzz(func(t *testing.T, wire, sizes []byte) {
		if len(wire) > 64<<10 || len(sizes) > 1024 {
			t.Skip()
		}
		whole := parseFixture(t, bytes.NewReader(wire))
		split := parseFixture(t, &fragmentReader{data: wire, sizes: sizes})
		if !bytes.Equal(whole, split) {
			t.Fatalf("SSE output depends on transport fragmentation")
		}
		if bytes.Contains(split, []byte(truncationfix.ToolName)) || !bytes.HasSuffix(split, []byte("data: [DONE]\n\n")) {
			t.Fatalf("unsafe or unterminated SSE: %q", split)
		}
	})
}
