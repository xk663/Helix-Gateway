package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"helix-gateway-local/internal/truncationfix"
)

func TestRepairOnlyPreservesUpstreamCORS(t *testing.T) {
	for _, status := range []int{200, 400} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const body = `{"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}]}`
			gateway := gatewayForTest(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Access-Control-Allow-Origin", "https://upstream.example")
				w.Header().Set("Access-Control-Expose-Headers", "X-Credit")
				w.Header().Set("Vary", "Accept-Encoding")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}, "/v1", "repair-only")
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest))
			req.Header.Set("Origin", "http://127.0.0.1:8000")
			rec := httptest.NewRecorder()
			gateway.ServeHTTP(rec, req)
			if rec.Code != status || rec.Body.String() != body ||
				rec.Header().Get("Access-Control-Allow-Origin") != "https://upstream.example" ||
				rec.Header().Get("Access-Control-Expose-Headers") != "X-Credit" || rec.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatalf("repair-only replaced a header or body: %d %v %s", rec.Code, rec.Header(), rec.Body)
			}
		})
	}
}

func TestEmbeddingsAreProxiedInEveryMode(t *testing.T) {
	const payload = `{"model":"embedding-test","input":["hello"]}`
	const answer = `{"data":[{"embedding":[0.25,0.75]}]}`
	for _, mode := range []string{"off", "repair-only", "synthetic"} {
		t.Run(mode, func(t *testing.T) {
			var forwarded atomic.Int32
			gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
				forwarded.Add(1)
				if r.Method == "OPTIONS" {
					w.Header().Set("Access-Control-Allow-Origin", "https://upstream.example")
					w.WriteHeader(204)
					return
				}
				if r.Method != "POST" || r.RequestURI != "/custom/v1/embeddings?trace=1" {
					t.Errorf("unexpected upstream routing: %s %s", r.Method, r.RequestURI)
					return
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != payload || r.Header.Get("Authorization") != "Bearer test" {
					t.Errorf("embeddings request changed: %q", body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, answer)
			}, "/custom/v1/", mode)
			preflight := httptest.NewRequest("OPTIONS", "/v1/embeddings", nil)
			preflight.Header.Set("Origin", "http://localhost:8000")
			rec := httptest.NewRecorder()
			gateway.ServeHTTP(rec, preflight)
			wantPreflightCalls := int32(0)
			wantOrigin := "http://localhost:8000"
			if mode == "off" {
				wantPreflightCalls = 1
				wantOrigin = "https://upstream.example"
			}
			if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != wantOrigin || forwarded.Load() != wantPreflightCalls {
				t.Fatalf("incorrect preflight: %d %v (%d forwarded)", rec.Code, rec.Header(), forwarded.Load())
			}
			req := httptest.NewRequest("POST", "/v1/embeddings?trace=1", strings.NewReader(payload))
			req.Header.Set("Authorization", "Bearer test")
			rec = httptest.NewRecorder()
			gateway.ServeHTTP(rec, req)
			if rec.Code != 200 || rec.Body.String() != answer || forwarded.Load() != wantPreflightCalls+1 {
				t.Fatalf("incorrect embeddings response: %d %s (%d forwarded)", rec.Code, rec.Body, forwarded.Load())
			}
			if stats := getStats(t, gateway); stats.Requests != 0 || stats.Interventions != 0 {
				t.Fatalf("embeddings counted as chat interventions: %+v", stats)
			}
		})
	}
}

func TestAssumeNoToolsSendsExactRequestOnce(t *testing.T) {
	t.Setenv("TRUNCATIONFIX_ASSUME_NO_TOOLS", "1")
	const payload = `{"model":"qwen-mock","messages":[{"role":"user","content":"test"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`
	var count atomic.Int32
	gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		body, _ := io.ReadAll(r.Body)
		if string(body) != payload {
			t.Errorf("forced bypass modified the client tools: %s", body)
		}
		w.WriteHeader(400)
		_, _ = io.WriteString(w, "unsupported capability: tools")
	}, "/v1", "synthetic")
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
	stats := getStats(t, gateway)
	if rec.Code != 400 || rec.Body.String() != "unsupported capability: tools" || count.Load() != 1 ||
		stats.Interventions != 0 || stats.GuardTrips != 0 || stats.FallbackReasons["assume_no_tools"] != 1 ||
		stats.FallbackReasons["upstream_rejected_tools"] != 0 {
		t.Fatalf("incorrect forced bypass: HTTP %d, calls %d, stats %+v", rec.Code, count.Load(), stats)
	}
}

func TestLengthOriginsAndGuardTripsAreIndependent(t *testing.T) {
	t.Setenv("TRUNCATIONFIX_MAX_LENGTH", "22")
	arguments := `{"content":"` + strings.Repeat("a", 80) + `"}`
	gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if bytes.Contains(raw, []byte("guard-only")) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "unexpected media type")
			return
		}
		_, _ = fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"content":"upstream"},"finish_reason":"length"},{"index":1,"message":{"tool_calls":[{"function":{"name":%q,"arguments":%q}}]},"finish_reason":"stop"}]}`, truncationfix.ToolName, arguments)
	}, "/v1", "synthetic")
	for _, model := range []string{"lengths", "guard-only"} {
		var payload map[string]any
		if err := json.Unmarshal([]byte(chatRequest), &payload); err != nil {
			t.Fatal(err)
		}
		payload["model"] = model
		raw, _ := json.Marshal(payload)
		rec := httptest.NewRecorder()
		gateway.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(raw)))
		if model == "lengths" {
			if rec.Code != 200 || bytes.Count(rec.Body.Bytes(), []byte(`"finish_reason":"length"`)) != 2 {
				t.Fatalf("unexpected processed choices: %d %s", rec.Code, rec.Body)
			}
		} else if rec.Code != 502 {
			t.Fatalf("guarded response status: %d", rec.Code)
		}
	}
	stats := getStats(t, gateway)
	if stats.LengthDecisions != 2 || stats.UpstreamLengthDecisions != 1 || stats.GatewayLengthDecisions != 1 ||
		stats.GuardTrips != 2 || stats.FallbackReasons["tool_arguments_limit"] != 1 || stats.FallbackReasons["upstream_content_type"] != 1 {
		t.Fatalf("unexpected length or guard totals: %+v", stats)
	}
}

func TestIdleWriteListenerPreservesTCPHalfClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := (idleWriteListener{Listener: listener, timeout: time.Second}).Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	writer, ok := accepted.(*idleWriteConn)
	if !ok {
		t.Fatal("accepted connection has no write timeout")
	}
	if err := writer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := client.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("expected FIN after half-close, got %d %v", n, err)
	}
}
