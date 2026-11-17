package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"helix-gateway-local/internal/truncationfix"
)

func gatewayForTest(t *testing.T, upstream http.HandlerFunc, base, mode string) http.Handler {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL + base)
	if err != nil {
		t.Fatal(err)
	}
	return newGateway(target, (truncationfix.Config{Enabled: true, WrapperMode: mode}).Resolve())
}

func getStats(t *testing.T, gateway http.Handler) statsSnapshot {
	t.Helper()
	r := httptest.NewRecorder()
	gateway.ServeHTTP(r, httptest.NewRequest("GET", "/stats", nil))
	var stats statsSnapshot
	if err := json.Unmarshal(r.Body.Bytes(), &stats); r.Code != 200 || err != nil {
		t.Fatalf("stats HTTP %d: %s (%v)", r.Code, r.Body, err)
	}
	return stats
}

func TestToolRejectionRetriesOriginalOnce(t *testing.T) {
	for _, tc := range []struct {
		label, initialBody, encoding       string
		retryStatus, wantStatus, wantCalls int
	}{
		{"400 mentions tools", `{"error":{"message":"tools are unsupported"}}`, "", 200, 200, 2},
		{"gzip 400 mentions function calling", `{"error":{"message":"function_call is unsupported"}}`, "gzip", 400, 400, 2},
		{"unrelated 400", `{"error":{"message":"invalid temperature"}}`, "", 200, 502, 1},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var mu sync.Mutex
			var requests [][]byte
			var encodings []string
			gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				defer mu.Unlock()
				requests = append(requests, body)
				encodings = append(encodings, r.Header.Get("Accept-Encoding"))
				w.Header().Set("Content-Type", "application/json")
				if len(requests) == 1 {
					w.Header().Set("X-Reflected-Tool", truncationfix.ToolName)
					if tc.encoding == "gzip" {
						w.Header().Set("Content-Encoding", "gzip")
						w.WriteHeader(400)
						compressed := gzip.NewWriter(w)
						_, _ = io.WriteString(compressed, tc.initialBody)
						_ = compressed.Close()
					} else {
						w.WriteHeader(400)
						_, _ = io.WriteString(w, tc.initialBody)
					}
					return
				}
				w.WriteHeader(tc.retryStatus)
				_, _ = io.WriteString(w, `{"error":{"message":"ordinary second response"}}`)
			}, "/v1", "synthetic")

			rec := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest))
			request.Header.Set("Accept-Encoding", "gzip, br")
			gateway.ServeHTTP(rec, request)
			mu.Lock()
			defer mu.Unlock()
			if rec.Code != tc.wantStatus || len(requests) != tc.wantCalls ||
				!bytes.Contains(requests[0], []byte(truncationfix.ToolName)) || encodings[0] != "identity" ||
				rec.Header().Get("X-Reflected-Tool") != "" {
				t.Fatalf("unexpected retry: HTTP %d, calls %d, encodings %v", rec.Code, len(requests), encodings)
			}
			stats := getStats(t, gateway)
			if tc.wantCalls == 2 {
				if string(requests[1]) != chatRequest || encodings[1] != "gzip, br" ||
					!strings.Contains(rec.Body.String(), "ordinary second response") ||
					stats.FallbackReasons["upstream_rejected_tools"] != 1 || stats.Requests != 1 {
					t.Fatalf("fall back did not preserve the original: %s %+v", rec.Body, stats)
				}
			} else if stats.FallbackReasons["upstream_rejected_tools"] != 0 {
				t.Fatalf("unrelated error triggered fallback: %+v", stats)
			}
		})
	}
}

func TestRepairOnlyFailures(t *testing.T) {
	for _, tc := range []struct {
		encoding, reason string
		status, want     int
		large            bool
	}{
		{"", "upstream_status", 400, 400, false},
		{"gzip", "upstream_gzip", 200, 502, false},
		{"br", "upstream_encoding", 200, 502, false},
		{"", "upstream_body_limit", 200, 502, true},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if string(raw) != chatRequest {
					t.Error("repair-only rewrote the request")
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.WriteHeader(tc.status)
				if tc.large {
					block := bytes.Repeat([]byte{'x'}, 32<<10)
					for sent := 0; sent <= maxBody; sent += len(block) {
						if _, err := w.Write(block); err != nil {
							return
						}
					}
				} else {
					_, _ = io.WriteString(w, "original upstream body")
				}
			}, "/v1", "repair-only")
			rec := httptest.NewRecorder()
			gateway.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest)))
			if rec.Code != tc.want || (tc.want == 400 && rec.Body.String() != "original upstream body") {
				t.Fatalf("unexpected repair-only HTTP %d: %s", rec.Code, rec.Body)
			}
			if stats := getStats(t, gateway); stats.FallbackReasons[tc.reason] != 1 {
				t.Fatalf("missing reason: %+v", stats)
			}
		})
	}
}

func TestStreamTerminalAndMissingDoneMetrics(t *testing.T) {
	for _, tc := range []struct {
		label, upstream      string
		missingDone, lengths uint64
		wantError            bool
	}{
		{"clearly finished without DONE", "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", 1, 0, false},
		{"missing finish", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n", 1, 1, false},
		{"damaged stream", "data: {\"choices\":[{\"index\":3,\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: bad\n\n", 0, 1, true},
		{"bad first event", "data: bad\n\n", 0, 1, true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			gateway := gatewayForTest(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.upstream)
			}, "/v1", "repair-only")
			rec := httptest.NewRecorder()
			gateway.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest)))
			out := rec.Body.String()
			if rec.Code != 200 || !strings.HasSuffix(out, "data: [DONE]\n\n") ||
				(strings.Contains(out, "upstream_stream_error") != tc.wantError) ||
				(strings.Contains(out, `"finish_reason":"length"`) != (tc.lengths > 0)) {
				t.Fatalf("bad terminal: %s", out)
			}
			if tc.wantError && strings.Index(out, "upstream_stream_error") > strings.Index(out, `"finish_reason":"length"`) {
				t.Fatalf("length event missing after error: %s", out)
			}
			stats := getStats(t, gateway)
			if stats.StreamMissingDone != tc.missingDone || stats.LengthDecisions != tc.lengths ||
				stats.FallbackReasons["stream_missing_done"] != 0 {
				t.Fatalf("incorrect stats: %+v", stats)
			}
		})
	}
}

type closedWriter struct{ *httptest.ResponseRecorder }

func (w closedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestUndeliveredTerminalNotCounted(t *testing.T) {
	gateway := gatewayForTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":%q}}]}}]}\n\n", truncationfix.ToolName)
	}, "/v1", "synthetic")
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(closedWriter{rec}, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest)))
	if rec.Body.Len() != 0 {
		t.Fatal("closed client unexpectedly received bytes")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gateway.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest)).WithContext(ctx))
	stats := getStats(t, gateway)
	if stats.LengthDecisions != 0 || stats.StreamMissingDone != 0 || stats.FallbackReasons["missing_finish_reason"] != 0 {
		t.Fatalf("undelivered terminals counted: %+v", stats)
	}
}

func TestOptionsAndLegacyCompletionPath(t *testing.T) {
	for base, expected := range map[string]string{"/": "/v1/completions", "/proxy%2Froute/v1///": "/proxy%2Froute/v1/completions"} {
		t.Run(base, func(t *testing.T) {
			var calls atomic.Int32
			gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, _ := io.ReadAll(r.Body)
				if r.URL.EscapedPath() != expected || r.URL.RawQuery != "trace=1" || string(body) != chatRequest {
					t.Errorf("wrong upstream path, query or body: %s?%s %s", r.URL.EscapedPath(), r.URL.RawQuery, body)
				}
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.WriteHeader(418)
				_, _ = io.WriteString(w, "legacy response")
			}, base, "synthetic")
			for origin, want := range map[string]int{"http://127.0.0.1:8000": 204, "https://example.test": 403} {
				req := httptest.NewRequest("OPTIONS", "/v1/completions", nil)
				req.Header.Set("Origin", origin)
				rec := httptest.NewRecorder()
				gateway.ServeHTTP(rec, req)
				if rec.Code != want || (want == 204 && rec.Header().Get("Access-Control-Allow-Origin") != origin) {
					t.Fatalf("bad preflight for %q: %d %v", origin, rec.Code, rec.Header())
				}
			}
			if calls.Load() != 0 {
				t.Fatal("preflight fetched upstream")
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/completions?trace=1", strings.NewReader(chatRequest))
			req.Header.Set("Origin", "http://localhost:8000")
			gateway.ServeHTTP(rec, req)
			if rec.Code != 418 || rec.Body.String() != "legacy response" ||
				rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:8000" || calls.Load() != 1 {
				t.Fatalf("legacy passthrough failed: HTTP %d %s", rec.Code, rec.Body)
			}
		})
	}
}
