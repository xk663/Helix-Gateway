package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"helix-gateway-local/internal/truncationfix"
)

func TestConcurrentStreams(t *testing.T) {
	const requests = 1000
	const payload = `{"model":"gpt-mock","stream":true,"messages":[{"role":"user","content":"Hello"}]}`
	var arrived atomic.Int32
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []struct {
				Function struct{ Name string }
			}
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Tools) != 1 {
			http.Error(w, "expected one injected tool", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": ready\r\n\r\n")
		w.(http.Flusher).Flush()
		if arrived.Add(1) == requests {
			close(release)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		name := request.Tools[0].Function.Name
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":%q,\"arguments\":\"{\\\"content\\\":\\\"hello\\\"}\"}}]}}]}\r\n\r\n", name)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\r\n\r\ndata: [DONE]\r\n\r\n")
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(newGateway(target, (truncationfix.Config{Enabled: true}).Resolve()))
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	client := gateway.Client()
	// Limit simultaneous connection setup to fit the local listener backlog.
	// Every successful stream stays open until all upstream handlers arrive.
	connecting := make(chan struct{}, 24)
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/chat/completions", bytes.NewBufferString(payload))
			if err != nil {
				t.Error(err)
				return
			}
			connecting <- struct{}{}
			response, err := client.Do(request)
			<-connecting
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK ||
				!bytes.Contains(raw, []byte("\"content\":\"hello\"")) ||
				!bytes.Contains(raw, []byte("\"finish_reason\":\"stop\"")) ||
				!bytes.Contains(raw, []byte("data: [DONE]")) ||
				bytes.Contains(raw, []byte("agw_emit_")) {
				t.Errorf("unexpected response HTTP %d %q (%v)", response.StatusCode, raw, err)
			}
		})
	}
	wg.Wait()
	if peak := arrived.Load(); peak != requests {
		t.Errorf("%d of %d streams arrived before release", peak, requests)
	}
}
