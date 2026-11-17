package truncationfix

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func newRequest(t *testing.T) *Request {
	t.Helper()
	raw := []byte(`{"model":"gemini-test","messages":[{"role":"user","content":"Hi"},{"role":"assistant","content":"Hello"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`)
	body, req, _ := (Config{Enabled: true}).Resolve().Prepare(raw)
	if req == nil || req.name != ToolName {
		t.Fatalf("unexpected transport: %+v", req)
	}
	obj := asObject(body)
	tools := asArray(obj["tools"])
	if len(tools) != 2 || !bytes.Equal(tools[0], asArray(asObject(raw)["tools"])[0]) {
		t.Fatal("existing tool was changed")
	}
	messages := asArray(obj["messages"])
	if len(messages) != 3 {
		t.Fatalf("Gemini prefill should add exactly one turn: %s", obj["messages"])
	}
	last := asObject(messages[2])
	if asString(last["role"]) != "user" ||
		asString(last["content"]) != "Continue from the last assistant message." ||
		!strings.Contains(asString(asObject(messages[0])["content"]), constraint) {
		t.Fatal("Gemini trailing turn was not normalized")
	}
	return req
}

func TestInjectionScope(t *testing.T) {
	a, b := newRequest(t), newRequest(t)
	if a.name != b.name {
		t.Fatal("transport name changed")
	}
	cfg := (Config{Enabled: true, DisabledModels: []string{"team/gpt-*"}}).Resolve()
	for _, raw := range []string{
		`{"model":"team/gpt-test","messages":[{"role":"user","content":"Hi"}]}`,
		`{"model":"tts-1","messages":[{"role":"user","content":"Hi"}]}`,
		`{"model":"gpt-test","messages":[{"role":"user","content":"Hi"}],"tool_choice":"none"}`,
		`{"model":"gpt-test","messages":[{"role":"user","content":"Hi"}],"response_format":{"type":"json_schema"}}`,
		`{"model":"gpt-test","messages":true}`,
	} {
		got, req, _ := cfg.Prepare([]byte(raw))
		if req != nil || string(got) != raw {
			t.Fatalf("changed ineligible request: %s", raw)
		}
	}
	t.Setenv("TRUNCATIONFIX_ENABLED", "false")
	if (Config{Enabled: true}).Resolve().Enabled {
		t.Fatal("environment did not override configured enablement")
	}
}

func tool(name, arg string, index int) object {
	fn := object{"arguments": encode(arg)}
	if name != "" {
		fn["name"] = encode(name)
	}
	return object{"index": encode(index), "function": encode(fn)}
}

func chunk(reason string, tools ...object) []byte {
	delta := object{}
	if len(tools) != 0 {
		delta["tool_calls"] = encode(tools)
	}
	choice := object{"delta": encode(delta), "index": encode(0)}
	if reason != "" {
		choice["finish_reason"] = encode(reason)
	}
	return encode(object{"id": encode("example"), "choices": encode([]object{choice})})
}

func readChoice(data []byte) object { return asObject(asArray(asObject(data)["choices"])[0]) }

func TestStreamingSplitArguments(t *testing.T) {
	req := newRequest(t)
	stream := req.Stream()
	fragments := []string{"{", "\"con", "tent\"", ":\"", "雪", "\\", "n", "\\uD8", "3D\\uDE", "03", " \\\"quoted\\\"", "\"", "}"}
	var assembled strings.Builder
	for i, fragment := range fragments {
		name := ""
		if i == 0 {
			name = req.name
		}
		result := mustChunk(t, stream, chunk("", tool(name, fragment, 0)))
		if len(result) == 0 {
			if i == 4 {
				t.Fatal("content was held until the end")
			}
			continue
		}
		delta := asObject(readChoice(result)["delta"])
		if len(asArray(delta["tool_calls"])) != 0 {
			t.Fatal("synthetic tool leaked")
		}
		text := asString(delta["content"])
		if i == 4 && text != "雪" {
			t.Fatal("content was held until the end")
		}
		assembled.WriteString(text)
	}
	last := readChoice(mustChunk(t, stream, chunk("tool_calls")))
	assembled.WriteString(asString(asObject(last["delta"])["content"]))
	if assembled.String() != "雪\n😃 \"quoted\"" || asString(last["finish_reason"]) != "stop" {
		t.Fatalf("assembled %q, final choice %s", assembled.String(), encode(last))
	}
}

func TestStreamingGenuineToolsAndLimits(t *testing.T) {
	req := newRequest(t)
	stream := req.Stream()
	c := readChoice(mustChunk(t, stream, chunk("", tool(req.name, "{\"content\":\"hello", 0), tool("agw_emit_not_this_request", "{}", 1))))
	delta := asObject(c["delta"])
	genuine := asArray(delta["tool_calls"])
	if len(genuine) != 1 || asString(asObject(asObject(genuine[0])["function"])["name"]) != "agw_emit_not_this_request" {
		t.Fatalf("genuine call changed: %s", encode(delta))
	}
	if number(asObject(genuine[0])["index"]) != 0 {
		t.Fatalf("index gap: %s", genuine[0])
	}
	c = readChoice(mustChunk(t, stream, chunk("tool_calls", tool("", "\"}", 0))))
	if asString(c["finish_reason"]) != "tool_calls" {
		t.Fatal("genuine finish reason changed")
	}
	req.cfg.MaxSyntheticToolLength = 20
	stream = req.Stream()
	result := mustChunk(t, stream, chunk("", tool(req.name, "{\"content\":\""+strings.Repeat("a", 100), 0)))
	if got := asString(asObject(readChoice(result)["delta"])["content"]); len(got) != 8 {
		t.Fatalf("bounded recovery got %d bytes", len(got))
	}
	result = mustChunk(t, stream, chunk("tool_calls"))
	if asString(readChoice(result)["finish_reason"]) != "length" {
		t.Fatal("size cap was hidden")
	}
}

func TestNonStreamingAndRepair(t *testing.T) {
	req := newRequest(t)
	message := object{"tool_calls": encode([]object{
		tool(req.name, "{\"content\":\"{\\\"nested\\\":[1,2\"}", 0),
		tool("lookup", "{}", 1),
	})}
	normal := encode(object{"choices": encode([]object{{"message": encode(message), "finish_reason": encode("tool_calls")}}), "usage": encode(map[string]int{"total_tokens": 7})})
	out, err := req.Complete(normal)
	if err != nil {
		t.Fatal(err)
	}
	answer := asObject(readChoice(out)["message"])
	if content := asString(answer["content"]); content != "{\"nested\":[1,2]}" || !json.Valid([]byte(content)) {
		t.Fatalf("nested JSON repair failed: %s", content)
	}
	if len(asArray(answer["tool_calls"])) != 1 || !bytes.Equal(asObject(out)["usage"], asObject(normal)["usage"]) {
		t.Fatal("genuine tool or usage was altered")
	}
	if suffix := repairSuffix("\x60\x60\x60go\nfunc main() {}", "moderate"); suffix != "\n\x60\x60\x60" {
		t.Fatalf("unexpected Markdown repair %q", suffix)
	}
	if suffix := repairSuffix("<root><item>text", "aggressive"); suffix != "</item></root>" {
		t.Fatalf("unexpected XML repair %q", suffix)
	}
}

func mustChunk(t *testing.T, stream *Stream, raw []byte) []byte {
	t.Helper()
	out, err := stream.Chunk(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOutOfOrderSyntheticAndGenuineToolIndices(t *testing.T) {
	req := newRequest(t)
	stream := req.Stream()
	seen := make(map[int64][]string)
	var recovered strings.Builder
	frames := [][]byte{
		chunk("", tool("lookup", "{", 7)),
		chunk("", tool(req.name, "{\"content\":\"", 2)),
		chunk("", tool(req.name, "{\"content\":\"甲", 0), tool("inspect", "{", 4)),
		chunk("", tool("", "乙\"}", 2), tool("", "\"x\":1}", 7)),
		chunk("tool_calls", tool("", "\"}", 0), tool("", "\"y\":2}", 4)),
	}
	for i, frame := range frames {
		out := mustChunk(t, stream, frame)
		if i == 1 {
			if len(out) != 0 {
				t.Fatalf("empty delta from hidden call: %s", out)
			}
			continue
		}
		choice := readChoice(out)
		delta := asObject(choice["delta"])
		recovered.WriteString(asString(delta["content"]))
		for _, raw := range asArray(delta["tool_calls"]) {
			call := asObject(raw)
			name := asString(asObject(call["function"])["name"])
			if name == req.name {
				t.Fatalf("hidden call remains: %s", raw)
			}
			seen[number(call["index"])] = append(seen[number(call["index"])], asString(asObject(call["function"])["arguments"]))
		}
		if i == len(frames)-1 && asString(choice["finish_reason"]) != "tool_calls" {
			t.Fatalf("genuine tool finish rewritten: %s", out)
		}
	}
	if len(seen) != 2 || strings.Join(seen[0], "") != "{\"x\":1}" ||
		strings.Join(seen[1], "") != "{\"y\":2}" || recovered.String() != "甲乙" {
		t.Fatalf("misrouted fragments: %v, recovered %q", seen, recovered.String())
	}
}
