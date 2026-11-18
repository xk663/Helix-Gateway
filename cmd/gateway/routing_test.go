package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestV1PassthroughPreservesUpstreamCORS(t *testing.T) {
	const origin = "https://upstream.example"
	for _, mode := range []string{"off", "repair-only", "synthetic"} {
		t.Run(mode, func(t *testing.T) {
			gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Expose-Headers", "X-Upstream")
				w.Header().Set("Vary", "Accept-Encoding")
				w.WriteHeader(http.StatusAccepted)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				_, _ = io.WriteString(w, r.Method+" "+r.RequestURI+" "+string(body))
			}, "/custom/v1/", mode)

			for _, route := range []struct{ method, path, body string }{
				{"GET", "/models", ""},
				{"POST", "/embeddings", "{}"},
				{"POST", "/images/generations", "{}"},
				{"POST", "/audio/speech", "{}"},
				{"POST", "/rerank", "{}"},
				{"OPTIONS", "/images/generations", ""},
			} {
				path := "/v1" + route.path + "?trace=1"
				req := httptest.NewRequest(route.method, path, strings.NewReader(route.body))
				req.Header.Set("Origin", "http://127.0.0.1:8000")
				rec := httptest.NewRecorder()
				gateway.ServeHTTP(rec, req)

				if rec.Code != http.StatusAccepted || rec.Body.String() != route.method+" /custom"+path+" "+route.body {
					t.Errorf("%s %s: unexpected upstream response %d %q", route.method, path, rec.Code, rec.Body)
				}
				if rec.Header().Get("Access-Control-Allow-Origin") != origin ||
					rec.Header().Get("Access-Control-Expose-Headers") != "X-Upstream" ||
					strings.Join(rec.Header().Values("Vary"), ",") != "Accept-Encoding" {
					t.Errorf("%s %s: upstream CORS changed: %v", route.method, path, rec.Header())
				}
			}
			if stats := getStats(t, gateway); stats.Requests != 0 || stats.Interventions != 0 {
				t.Fatalf("passthrough requests entered chat processing: %+v", stats)
			}
		})
	}
}

func TestSyntheticChatLoopbackCORS(t *testing.T) {
	gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "https://upstream.example")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}]}`)
	}, "/v1", "synthetic")
	const origin = "http://localhost:8000"
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest))
	req.Header.Set("Origin", origin)
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != origin ||
		rec.Header().Get("Vary") != "Origin" || getStats(t, gateway).SyntheticRequests != 1 {
		t.Fatalf("synthetic chat lost loopback CORS: %d %v", rec.Code, rec.Header())
	}
}
