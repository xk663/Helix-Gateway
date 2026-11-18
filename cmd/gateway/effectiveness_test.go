package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"helix-gateway-local/internal/truncationfix"
)

func TestAdaptiveBudgetAndModelStats(t *testing.T) {
	t.Setenv("TRUNCATIONFIX_MAX_TOKENS_CEILING", "8192")
	caps := make(chan int, 4)
	gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
		var chat chatParameters
		if err := json.NewDecoder(r.Body).Decode(&chat); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		field, cap := chat.cap()
		if field != "max_completion_tokens" {
			t.Errorf("rewrote the budget field: %q", field)
		}
		caps <- cap
		w.Header().Set("Content-Type", "application/json")
		reason := "stop"
		switch cap {
		case 300:
			reason = "length"
		case 600:
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"maximum context length is 4096"}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"mentions %s in prose"},"finish_reason":%q}]}`, truncationfix.ToolName, reason)
	}, "/v1", "repair-only")
	body := strings.Replace(chatRequest, `"messages"`, `"max_completion_tokens":300,"messages"`, 1)
	for i, want := range []int{200, 200, 400, 200} {
		rec := httptest.NewRecorder()
		gateway.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != want {
			t.Fatalf("request %d: expected HTTP %d, got %d: %s", i+1, want, rec.Code, rec.Body)
		}
	}
	close(caps)
	var received []int
	for budget := range caps {
		received = append(received, budget)
	}
	if !slices.Equal(received, []int{300, 300, 600, 450}) {
		t.Fatalf("actual budgets: %v", received)
	}
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, httptest.NewRequest("GET", "/stats?by=model", nil))
	var result struct {
		statsSnapshot
		ByModel map[string]modelStats `json:"by_model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	model := result.ByModel["qwen-mock"]
	if model != result.modelStats || model.Requests != 4 || model.UpstreamCalls != 4 ||
		model.UpstreamCallsPerRequest != 1 || model.UpstreamLengthRate != 2.0/3.0 ||
		model.ObservedRequests != 3 || model.UpstreamLengthRequests != 2 ||
		model.ClientCapSuspected != 2 || model.TokenCapIncreases != 2 {
		t.Fatalf("unexpected model statistics: %+v", result)
	}
}

func TestLearnedOutputLimitDoesNotTreatContextWindowAsOutputLimit(t *testing.T) {
	policy := &tokenPolicy{models: map[retryKey]tokenPolicyEntry{}, ceiling: 8192}
	var first, other retryKey
	other[0] = 1
	policy.reject(first, 600, 600, rejectionDetail{Message: "max_tokens must be ≤ 400"})
	policy.observe(first, 400, true)
	policy.observe(first, 400, true)
	if got := policy.cap(first, 9000); got != 400 {
		t.Fatalf("known hard limit ignored: %d", got)
	}
	policy.reject(other, 600, 600, rejectionDetail{Message: "maximum context length is 500"})
	if got := policy.cap(other, 600); got != 600 {
		t.Fatalf("context window clamped caller limit: %d", got)
	}
}

func TestSuccessfulFallbackCachesCapabilityAcrossMessages(t *testing.T) {
	t.Setenv("TRUNCATIONFIX_FORCE_TOOL_CHOICE", "1")
	for _, onlyChoice := range []bool{false, true} {
		t.Run(fmt.Sprint("forced_choice_only_", onlyChoice), func(t *testing.T) {
			gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Tools  []json.RawMessage `json:"tools"`
					Choice json.RawMessage   `json:"tool_choice"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				w.Header().Set("Content-Type", "application/json")
				if len(payload.Tools) == 1 && string(payload.Choice) != `"auto"` {
					if !strings.Contains(string(payload.Choice), truncationfix.ToolName) {
						t.Errorf("expected forced synthetic choice: %s", payload.Choice)
					}
					w.WriteHeader(400)
					if onlyChoice {
						_, _ = io.WriteString(w, `{"error":{"code":"unsupported_parameter","param":"tool_choice"}}`)
					} else {
						_, _ = io.WriteString(w, `{"error":{"code":"unsupported_tools"}}`)
					}
					return
				}
				if len(payload.Tools) == 1 {
					_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"tool_calls":[{"function":{"name":%q,"arguments":"{\"content\":\"from auto tool\"}"}}]},"finish_reason":"tool_calls"}]}`, truncationfix.ToolName)
					return
				}
				if len(payload.Tools) == 2 && string(payload.Choice) != `"auto"` {
					t.Errorf("genuine tools must retain auto choice: %s", payload.Choice)
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
			}, "/v1", "synthetic")
			for i, auth := range []string{"a", "a", "b"} {
				req := httptest.NewRequest("POST", "/v1/chat/completions",
					strings.NewReader(strings.ReplaceAll(chatRequest, "hello", fmt.Sprint(i))))
				req.Header.Set("Authorization", "Bearer "+auth)
				rec := httptest.NewRecorder()
				gateway.ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("fallback request %d: HTTP %d %s", i+1, rec.Code, rec.Body)
				}
				if onlyChoice && (!strings.Contains(rec.Body.String(), "from auto tool") || strings.Contains(rec.Body.String(), truncationfix.ToolName)) {
					t.Fatalf("auto did not preserve the synthetic channel: %s", rec.Body)
				}
				want := []uint64{2, 3, 5}[i]
				if got := getStats(t, gateway).UpstreamCalls; got != want {
					t.Fatalf("request %d: %d total upstream calls, want %d", i+1, got, want)
				}
			}
			if onlyChoice {
				rec := httptest.NewRecorder()
				body := strings.Replace(chatRequest, `"messages"`, `"tools":[{"type":"function","function":{"name":"real"}}],"messages"`, 1)
				req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer a")
				gateway.ServeHTTP(rec, req)
				if stats := getStats(t, gateway); rec.Code != 200 || stats.UpstreamCalls != 6 ||
					stats.FallbackReasons["upstream_tool_retry_suppressed"] != 0 ||
					stats.FallbackReasons["upstream_rejected_forced_choice"] != 2 || stats.ToolHits != 3 || stats.ToolUnused != 1 {
					t.Fatalf("forced-choice fallback affected genuine tools: HTTP %d %+v", rec.Code, stats)
				}
			}
		})
	}
}

func TestStreamingMergerSuppressesReportedFullRepeat(t *testing.T) {
	const sentence = "Hello world, this is a long enough sentence to test dedup properly."
	t.Setenv("TRUNCATIONFIX_CONTINUE", "0")
	gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", sentence)
		for i, part := range []string{`{"content":"` + sentence[:32], sentence[32:] + `"}`} {
			function := chatObject{"arguments": marshalJSON(part)}
			if i == 0 {
				function["name"] = marshalJSON(truncationfix.ToolName)
			}
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":%s}]},\"finish_reason\":null}]}\n\n", marshalJSON(function))
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}, "/v1", "synthetic")
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest)))
	body := rec.Body.String()
	stats := getStats(t, gateway)
	if rec.Code != 200 || strings.Count(body, sentence) != 1 ||
		strings.Contains(body, truncationfix.ToolName) || !strings.Contains(body, `"finish_reason":"stop"`) ||
		stats.DeduplicatedBytes != uint64(len(sentence)) || stats.RecoveredBytes != 0 {
		t.Fatalf("full repeat was not suppressed: %s (%+v)", body, stats)
	}
}
