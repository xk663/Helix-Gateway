package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"helix-gateway-local/internal/truncationfix"
)

func TestOffPreservesUpstreamCORSAndVary(t *testing.T) {
	gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "" {
			t.Errorf("off introduced Accept-Encoding: %q", r.Header.Get("Accept-Encoding"))
		}
		if r.URL.EscapedPath() != "/api/chat/completions" {
			t.Errorf("unexpected upstream path: %s", r.URL.EscapedPath())
		}
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if string(body) != chatRequest {
				t.Errorf("off changed body: %s", body)
			}
		}
		w.Header().Set("Access-Control-Allow-Origin", "https://upstream.example")
		w.Header().Set("Access-Control-Expose-Headers", "X-Upstream")
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, "upstream error")
	}, "/api/", "off")
	for _, method := range []string{"POST", "OPTIONS"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v1/chat/completions", strings.NewReader(chatRequest))
		req.Header.Set("Origin", "http://127.0.0.1:8000")
		gateway.ServeHTTP(rec, req)
		if rec.Code != 400 || rec.Body.String() != "upstream error" ||
			rec.Header().Get("Access-Control-Allow-Origin") != "https://upstream.example" ||
			rec.Header().Get("Access-Control-Expose-Headers") != "X-Upstream" ||
			strings.Join(rec.Header().Values("Vary"), ",") != "Accept-Encoding" {
			t.Errorf("%s off mutated upstream response: %d %v %q", method, rec.Code, rec.Header(), rec.Body)
		}
	}
}

func TestExplicitToolRejection(t *testing.T) {
	cases := map[string]bool{
		`{"error":{"code":"unsupported_capability","param":"tools"}}`:                                    true,
		"unsupported capability: tools":                                                                  true,
		"Unknown parameter: 'tools[0].function'":                                                         false,
		"Model does not support tools.":                                                                  true,
		"Tool calling is not supported by this model":                                                    true,
		"Function calling unsupported":                                                                   true,
		"Unrecognized request argument supplied: 'tools'":                                                true,
		"Unrecognized request argument: 'tools'":                                                         true,
		"invalid function arguments":                                                                     false,
		"tool_choice is not supported":                                                                   false,
		"function 'x' not found":                                                                         false,
		"this model does not support tool arguments":                                                     false,
		"tools: invalid schema at function.parameters":                                                   false,
		`{"error":{"code":"unsupported_parameter","param":"tools"}}`:                                     true,
		`{"error":{"code":"unsupported_parameter","param":"tool_choice","message":"tools unsupported"}}`: false,
		`{"error":{"code":"unsupported_function_calling"}}`:                                              true,
		`{"error":{"code":400,"message":"Model does not support tools"}}`:                                true,
		`{"error":"Model does not support tools"}`:                                                       true,
		`{"request":{"message":"tools unsupported"}}`:                                                    false,
	}
	for message, want := range cases {
		if got := explicitToolRejection([]byte(message)); got != want {
			t.Errorf("classification of %q: %v, wanted %v", message, got, want)
		}
	}
}

type closeTracker struct {
	io.Reader
	closed bool
}

func (r *closeTracker) Close() error {
	r.closed = true
	return nil
}

func TestToolErrorProbeRestoresRawBody(t *testing.T) {
	identity := []byte(`{"error":{"message":"invalid function arguments"}}`)
	var compressed bytes.Buffer
	zip := gzip.NewWriter(&compressed)
	_, _ = zip.Write([]byte("Model does not support tools"))
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		encoding string
		wire     []byte
		match    bool
	}{
		{"", identity, false},
		{"gzip", compressed.Bytes(), true},
		{"gzip", []byte("not a gzip header"), false},
		{"", bytes.Repeat([]byte("tools unsupported "), toolErrorLimit/8), false},
	} {
		upstream := &closeTracker{Reader: bytes.NewReader(tc.wire)}
		response := &http.Response{Header: make(http.Header), Body: upstream}
		response.Header.Set("Content-Encoding", tc.encoding)
		if found := toolRejection(response); found != tc.match {
			t.Fatalf("probe returned %v, wanted %v", found, tc.match)
		}
		got, err := io.ReadAll(response.Body)
		if err != nil || !bytes.Equal(got, tc.wire) || upstream.closed {
			t.Fatalf("probe did not preserve wire body: %d bytes (%v)", len(got), err)
		}
		if err := response.Body.Close(); err != nil || !upstream.closed {
			t.Fatal("wrapped closer did not close upstream")
		}
	}
}

func TestNegativeRetryCacheAfterSecond400(t *testing.T) {
	var mu sync.Mutex
	var wrapped []bool
	gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, _ := io.ReadAll(r.Body)
		hasTool := bytes.Contains(data, []byte(truncationfix.ToolName))
		mu.Lock()
		wrapped = append(wrapped, hasTool)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		if hasTool {
			_, _ = io.WriteString(w, `{"error":{"code":"unsupported_tools"}}`)
		} else {
			_, _ = io.WriteString(w, `{"error":{"message":"bad original message"}}`)
		}
	}, "/v1", "synthetic")
	for _, auth := range []string{"Bearer local-a", "Bearer local-a", "Bearer local-b"} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest))
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		gateway.ServeHTTP(rec, req)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad original message") {
			t.Fatalf("expected ordinary upstream error: %d %s", rec.Code, rec.Body)
		}
	}
	mu.Lock()
	got := append([]bool(nil), wrapped...)
	mu.Unlock()
	want := []bool{true, false, false, true, false}
	if len(got) != len(want) {
		t.Fatalf("unexpected calls: %v", got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("unexpected call sequence: %v", got)
		}
	}
	stats := getStats(t, gateway)
	if stats.FallbackReasons["upstream_rejected_tools"] != 2 || stats.FallbackReasons["upstream_tool_retry_suppressed"] != 1 {
		t.Fatalf("unexpected fallback reasons: %+v", stats)
	}
}

func TestRetryCacheExpirationAndCapacity(t *testing.T) {
	cache := &toolRetryCache{entries: make(map[retryKey]time.Time)}
	now := time.Unix(1, 0)
	var first retryKey
	cache.remember(first, now)
	if !cache.contains(first, now.Add(retryCacheTTL-time.Millisecond)) || cache.contains(first, now.Add(retryCacheTTL)) {
		t.Fatal("retry suppression did not expire")
	}
	for i := 0; i < retryCacheSize*2; i++ {
		var key retryKey
		key[0], key[1] = byte(i), byte(i>>8)
		cache.remember(key, now.Add(time.Duration(i)*time.Millisecond))
	}
	if len(cache.entries) != retryCacheSize || cache.contains(first, now.Add(time.Second)) {
		t.Fatal("retry cache exceeded its bound or retained its oldest entry")
	}
}

func TestEscapedIncomingPathsRemainEncoded(t *testing.T) {
	u, err := url.Parse("/v%31/custom%2froute/%25escaped?trace=1")
	if err != nil {
		t.Fatal(err)
	}
	path, wire := upstreamPath("/provider%2Froute/v1", u)
	if path != "/provider/route/v1/custom/route/%escaped" ||
		wire != "/provider%2Froute/v1/custom%2froute/%25escaped" {
		t.Fatalf("decoded path %q, raw path %q", path, wire)
	}
	gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != "/provider%2Froute/v1/mod%65ls?trace=1" {
			t.Errorf("upstream wire path was changed: %s", r.RequestURI)
		}
		w.WriteHeader(204)
	}, "/provider%2Froute/v1", "off")
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, httptest.NewRequest("GET", "/v%31/mod%65ls?trace=1", nil))
	if rec.Code != 204 {
		t.Fatalf("escaped route HTTP %d: %s", rec.Code, rec.Body)
	}
}

func TestSocketWriteDeadlineBoundsBlockedWriteAndClears(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	conn := &idleWriteConn{Conn: server, timeout: 25 * time.Millisecond}
	n, err := conn.Write([]byte("blocked"))
	netErr, ok := err.(net.Error)
	if n != 0 || !ok || !netErr.Timeout() {
		t.Fatalf("blocked write returned %d, %v", n, err)
	}

	received := make(chan string, 1)
	go func() {
		buf := make([]byte, len("next"))
		n, _ := io.ReadFull(client, buf)
		received <- string(buf[:n])
	}()
	if _, err := io.WriteString(server, "next"); err != nil {
		t.Fatalf("write after timeout: %v", err)
	}
	if got := <-received; got != "next" {
		t.Fatalf("got %q", got)
	}
}
