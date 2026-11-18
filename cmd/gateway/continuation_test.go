package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"helix-gateway-local/internal/truncationfix"
)

func TestAdaptivePromotesAndDemotesWithEvidence(t *testing.T) {
	t.Setenv("TRUNCATIONFIX_MAX_TOKENS_CEILING", "off")
	t.Setenv("TRUNCATIONFIX_CONTINUE", "0")
	var observed atomic.Int32
	gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		turn := observed.Add(1)
		wantTool := turn >= 4 && turn <= 6
		hasTool := strings.Contains(string(raw), truncationfix.ToolName)
		if hasTool != wantTool {
			t.Errorf("turn %d synthetic channel %v, want %v", turn, hasTool, wantTool)
		}
		reason := "stop"
		if turn <= 3 {
			reason = "length"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"text"},"finish_reason":%q}]}`, reason)
	}, "/v1", "adaptive")
	for range 7 {
		rec := httptest.NewRecorder()
		gateway.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest)))
		if rec.Code != http.StatusOK {
			t.Fatalf("unexpected response %d %s", rec.Code, rec.Body)
		}
	}
	stats := getStats(t, gateway)
	if stats.RepairOnlyRequests != 4 || stats.SyntheticRequests != 3 || stats.UpstreamCalls != 7 ||
		stats.FallbackReasons["synthetic_enabled_after_length"] != 1 ||
		stats.FallbackReasons["synthetic_disabled_after_misses"] != 1 {
		t.Fatalf("unexpected mode statistics: %+v", stats)
	}
}

func TestContinuationStitchesSSEWithBudgetAndUsage(t *testing.T) {
	const overlap = "这小段正文只应出现一次"
	const partial = "段首。" + overlap
	const addition = "，接下来的字。"
	const requestBody = `{"model":"qwen-mock","messages":[{"role":"user","content":"hello"}],"stream":true,"max_tokens":120,"temperature":0.3,"stop":["END"],"stream_options":{"include_usage":true}}`
	for _, tc := range []struct {
		name, tokens, wantText, wantReason, wantTrailer string
		wantCalls, secondStatus                         int
	}{
		{"success", "250", partial + addition, "stop", "1", 2, 200},
		{"budget exhausted", "180", partial, "length", "0", 1, 200},
		{"upstream rate limit", "250", partial, "length", "0", 2, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRUNCATIONFIX_CONTINUE", "1")
			t.Setenv("TRUNCATIONFIX_CONTINUE_TOKENS", tc.tokens)
			t.Setenv("TRUNCATIONFIX_MAX_TOKENS_CEILING", "off")
			var calls atomic.Int32
			gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				var request chatObject
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if call == 1 {
					emitContinuationFixture(w, "first", []string{partial}, "length", 10, 100)
					return
				}
				var messages []map[string]string
				_ = json.Unmarshal(request["messages"], &messages)
				if len(messages) != 3 || messages[1]["content"] != partial || messages[1]["role"] != "assistant" ||
					!strings.HasPrefix(messages[2]["content"], "Continue exactly") ||
					string(request["max_tokens"]) != "120" || string(request["temperature"]) != "0.3" ||
					string(request["stop"]) != `["END"]` {
					t.Errorf("bad continuation request %+v", request)
				}
				if tc.secondStatus != 200 {
					w.WriteHeader(tc.secondStatus)
					return
				}
				emitContinuationFixture(w, "second", []string{"这小段", "正文只应出现一次" + addition}, "stop", 31, 12)
			}, "/v1", "repair-only")
			server := httptest.NewServer(gateway)
			defer server.Close()
			resp, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(requestBody))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var text strings.Builder
			var reasons []string
			var usage *completionUsage
			done := 0
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				payload, ok := strings.CutPrefix(scanner.Text(), "data: ")
				if !ok {
					continue
				}
				if payload == "[DONE]" {
					done++
					continue
				}
				var event struct {
					ID      string
					Choices []struct {
						Delta  struct{ Content string }
						Reason string `json:"finish_reason"`
					}
					Usage *completionUsage
				}
				if err := json.Unmarshal([]byte(payload), &event); err != nil || event.ID != "first" {
					t.Fatalf("invalid stream event: %s (%v)", payload, err)
				}
				if event.Usage != nil {
					if usage != nil {
						t.Fatalf("duplicate usage: %+v", event.Usage)
					}
					usage = event.Usage
				}
				for _, choice := range event.Choices {
					text.WriteString(choice.Delta.Content)
					if choice.Reason != "" {
						reasons = append(reasons, choice.Reason)
					}
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 200 || text.String() != tc.wantText || !slices.Equal(reasons, []string{tc.wantReason}) ||
				done != 1 || resp.Trailer.Get(continuedTrailer) != tc.wantTrailer || int(calls.Load()) != tc.wantCalls {
				t.Fatalf("HTTP %d, text %q, finish %v, DONE %d, trailer %v, calls %d",
					resp.StatusCode, &text, reasons, done, resp.Trailer, calls.Load())
			}
			stats := getStats(t, gateway)
			if tc.wantTrailer == "1" {
				if usage == nil || *usage != (completionUsage{Prompt: 41, Completion: 112, Total: 153}) ||
					stats.ContinuationHops != 1 || stats.RecoveredBytes != uint64(len(addition)) ||
					stats.DeduplicatedBytes != uint64(len(overlap)) || stats.UpstreamCalls != 2 {
					t.Fatalf("continuation totals: usage %+v, stats %+v", usage, stats)
				}
			} else if usage == nil || usage.Completion != 100 || stats.ContinuationHops != 0 {
				t.Fatalf("unexpected failed-hop totals: usage %+v, stats %+v", usage, stats)
			}
		})
	}
}

func TestContinuationRequiresUpstreamLengthAndRespectsHopLimit(t *testing.T) {
	const requestBody = `{"model":"qwen-mock","messages":[{"role":"user","content":"hello"}],"stream":true,"max_tokens":120}`
	t.Setenv("TRUNCATIONFIX_CONTINUE", "2")
	t.Setenv("TRUNCATIONFIX_CONTINUE_TOKENS", "600")
	t.Setenv("TRUNCATIONFIX_MAX_TOKENS_CEILING", "off")
	for _, gatewayLength := range []bool{false, true} {
		t.Run(fmt.Sprint("gateway_length_", gatewayLength), func(t *testing.T) {
			var calls atomic.Int32
			gateway := gatewayForTest(t, func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if gatewayLength {
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"first\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":%q,\"arguments\":%q}}]},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
						truncationfix.ToolName, `{"content":"already received`)
					return
				}
				emitContinuationFixture(w, "first", []string{fmt.Sprintf("segment-%d ", call)}, "length", 1, 120)
			}, "/v1", "synthetic")
			rec := httptest.NewRecorder()
			gateway.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(requestBody)))
			output := rec.Body.String()
			wantCalls := 3
			if gatewayLength {
				wantCalls = 1
			}
			stats := getStats(t, gateway)
			if rec.Code != 200 || strings.Count(output, `"finish_reason":"length"`) != 1 ||
				strings.Count(output, "data: [DONE]") != 1 || int(calls.Load()) != wantCalls ||
				stats.ContinuationHops != uint64(wantCalls-1) {
				t.Fatalf("unexpected output: %s, HTTP %d, calls %d, stats %+v", output, rec.Code, calls.Load(), stats)
			}
		})
	}
}

func emitContinuationFixture(w http.ResponseWriter, id string, fragments []string, finish string, input, output int) {
	for _, text := range fragments {
		_, _ = fmt.Fprintf(w, "data: {\"id\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", id, text)
	}
	_, _ = fmt.Fprintf(w, "data: {\"id\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\n", id, finish)
	_, _ = fmt.Fprintf(w, "data: {\"id\":%q,\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n",
		id, input, output, input+output)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}
