package truncationfix

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestStableToolAndSystemMerge(t *testing.T) {
	cfg := (Config{Enabled: true}).Resolve()
	raw := []byte(`{"model":"qwen-test","max_tokens":37,"messages":[{"role":"system","content":[{"type":"text","text":"roleplay","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"Hi"}]}`)
	first, request, reason := cfg.Prepare(raw)
	second, _, _ := cfg.Prepare(raw)
	if request == nil || reason != "" || !bytes.Equal(first, second) {
		t.Fatal("tool schema or instruction changed across identical requests")
	}
	body := asObject(first)
	messages := asArray(body["messages"])
	system := asObject(messages[0])
	parts := asArray(system["content"])
	if len(messages) != 2 || asString(system["role"]) != "system" || len(parts) != 2 ||
		!bytes.Contains(parts[0], []byte("cache_control")) || string(body["max_tokens"]) != "37" {
		t.Fatalf("unexpected system merge: %s", first)
	}

	raw = []byte(`{"model":"qwen-test","messages":[{"role":"user","content":"Hi"}],"tools":[{"type":"function","function":{"name":"agw_emit_response"}}]}`)
	result, fix, reason := cfg.Prepare(raw)
	if fix != nil || reason != "tool_name_collision" || !bytes.Equal(raw, result) {
		t.Fatal("client tool with the reserved name was intercepted")
	}
	custom := bytes.ReplaceAll(raw, []byte(`"function"`), []byte(`"custom"`))
	result, fix, reason = cfg.Prepare(custom)
	if fix != nil || reason != "tool_name_collision" || !bytes.Equal(custom, result) {
		t.Fatal("client custom tool with the reserved name was intercepted")
	}
}

func TestBlacklistAndBedrockNames(t *testing.T) {
	cfg := (Config{Enabled: true}).Resolve()
	models := []string{
		"deepseek-chat", "qwen3", "glm-4", "kimi-k2", "grok-4", "lmstudio/local-model",
		"anthropic.claude-sonnet-4-5-20250929-v1:0",
		"arn:aws-us-gov:bedrock:us-gov-west-1:000000000000:inference-profile/us.anthropic.claude-sonnet-4-5-v1:0",
		"claude.sonnet-4",
	}
	for _, model := range models {
		if !cfg.wraps(model) {
			t.Errorf("model %q excluded", model)
		}
	}
	cfg.DisabledModels = []string{"claude-*", "lmstudio/*"}
	for _, model := range models[5:] {
		if cfg.wraps(model) {
			t.Errorf("model %q ignored the blacklist", model)
		}
	}
	for _, model := range []string{"gemini-3-flash-image", "agy-gemini-tts-preview", "vendor/qwen3-embedding", "whisper-1", "gpt-realtime", "gpt-3.5-turbo-instruct", "text-davinci-003"} {
		if cfg.wraps(model) {
			t.Errorf("non-text model %q admitted", model)
		}
	}
}

func TestRepairOnlyAndMultipleChoices(t *testing.T) {
	t.Setenv("TRUNCATIONFIX_ASSUME_NO_TOOLS", "1")
	raw := []byte(`{"model":"lmstudio/roleplay","messages":[{"role":"user","content":"Go on"}],"max_tokens":25,"tool_choice":"none"}`)
	got, req, reason := (Config{Enabled: true, WrapperMode: "repair-only", RepairLevel: "moderate"}).Resolve().Prepare(raw)
	if req == nil || req.Synthetic() || reason != "" || !bytes.Equal(got, raw) {
		t.Fatal("repair-only modified the outbound request")
	}
	wire := []byte(`{"id":"reply","choices":[{"index":1,"delta":{"content":"别改我的话"},"finish_reason":"stop"},{"index":0,"delta":{"content":"{\"line\": [\"你好\""},"finish_reason":"length"}]}`)
	out := mustChunk(t, req.Stream(), wire)
	choices := asArray(asObject(out)["choices"])
	if got := asString(asObject(asObject(choices[0])["delta"])["content"]); got != "别改我的话" {
		t.Fatalf("ordinary text mutated: %q", got)
	}
	if got := asString(asObject(asObject(choices[1])["delta"])["content"]); got != "{\"line\": [\"你好\"]}" || !json.Valid([]byte(got)) {
		t.Fatalf("did not append closures: %q", got)
	}
	report := req.Report()
	if report.UpstreamLength != 1 || report.GatewayLength != 0 || report.Repairs != 1 || report.RecoveredBytes != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestMissingFinishAndChoiceIsolation(t *testing.T) {
	req := newRequest(t)
	stream := req.Stream()
	data := encode(object{"id": encode("multi"), "choices": encode([]object{
		{"index": encode(0), "delta": encode(object{"tool_calls": encode([]object{tool(ToolName, "{\"content\":\"甲", 0)})})},
		{"index": encode(1), "delta": encode(object{"content": encode("乙")})},
	})})
	out := mustChunk(t, stream, data)
	if bytes.Contains(out, []byte(ToolName)) || !strings.Contains(string(out), "甲") {
		t.Fatalf("bad synthetic delta: %s", out)
	}
	end, terminalReport := stream.End(false)
	terminal := asArray(asObject(end)["choices"])
	if len(terminal) != 2 || asString(asObject(terminal[0])["finish_reason"]) != "length" ||
		asString(asObject(terminal[1])["finish_reason"]) != "length" {
		t.Fatalf("unexpected terminal choices: %s", encode(terminal))
	}
	if repeat, _ := stream.End(false); len(repeat) != 0 || req.Report().GatewayLength != 0 || terminalReport.GatewayLength != 2 {
		t.Fatal("terminal counters counted a choice twice")
	}
	req.Record(terminalReport)
	if req.Report().Reasons["missing_finish_reason"] != 2 {
		t.Fatal("acknowledged terminal metrics were lost")
	}
}

func TestDecoderDoesNotEmitHalfUTF8Rune(t *testing.T) {
	var arg arguments
	input := "{\"content\":\"雪\"}"
	split := strings.Index(input, "雪") + 1
	if first := arg.feed(input[:split], 128); first != "" {
		t.Fatalf("emitted incomplete UTF-8: %q", first)
	}
	if next := arg.feed(input[split:], 128); next != "雪" {
		t.Fatalf("failed to reassemble UTF-8: %q", next)
	}
	if tail, complete := arg.finish("conservative"); !complete || tail != "" {
		t.Fatalf("unexpected terminal bytes: %q, complete=%v", tail, complete)
	}
}
