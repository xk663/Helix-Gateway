package truncationfix

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
)

type object map[string]json.RawMessage

func asObject(raw json.RawMessage) object {
	var value object
	_ = json.Unmarshal(raw, &value)
	return value
}

func asArray(raw json.RawMessage) []json.RawMessage {
	var value []json.RawMessage
	_ = json.Unmarshal(raw, &value)
	return value
}

func asString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func encode(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

type Request struct {
	name string
	cfg  Config
}

func (r *Request) BufferSize() int {
	return r.cfg.StreamBufferSize
}

// Inject preserves the original bytes when wrapping is disabled or inapplicable.
func (cfg Config) Inject(raw []byte) ([]byte, *Request) {
	if !cfg.Enabled {
		return raw, nil
	}
	var body object
	if json.Unmarshal(raw, &body) != nil {
		return raw, nil
	}
	model := asString(body["model"])
	if !cfg.wraps(model) {
		return raw, nil
	}
	messages := asArray(body["messages"])
	if len(messages) == 0 {
		return raw, nil
	}
	choice := bytes.TrimSpace(body["tool_choice"])
	if len(choice) > 0 && !bytes.Equal(choice, []byte("null")) && asString(choice) != "auto" {
		return raw, nil
	}
	for _, key := range []string{"functions", "function_call", "response_format", "audio"} {
		if value := bytes.TrimSpace(body[key]); len(value) > 0 && !bytes.Equal(value, []byte("null")) {
			if key == "response_format" && asString(asObject(value)["type"]) == "text" {
				continue
			}
			return raw, nil
		}
	}
	tools := asArray(body["tools"])
	if value := bytes.TrimSpace(body["tools"]); tools == nil && len(value) > 0 && !bytes.Equal(value, []byte("null")) {
		return raw, nil
	}
	var nonce [12]byte
	_, _ = rand.Read(nonce[:])
	name := "agw_emit_" + hex.EncodeToString(nonce[:])
	for _, tool := range tools {
		if asString(asObject(asObject(tool)["function"])["name"]) == name {
			return raw, nil
		}
	}
	tools = append(tools, encode(map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": "Send the full final answer to the user as the content string. Use this for the final answer, not for intermediate actions.",
			"parameters": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"content": map[string]string{"type": "string"}},
				"required":             []string{"content"},
				"additionalProperties": false,
			},
		},
	}))
	body["tools"] = encode(tools)
	body["tool_choice"] = encode("auto")

	shortModel := strings.ToLower(model[strings.LastIndexByte(model, '/')+1:])
	last := asObject(messages[len(messages)-1])
	if strings.HasPrefix(shortModel, "gemini-") && asString(last["role"]) == "assistant" &&
		len(asArray(last["tool_calls"])) == 0 && len(asObject(last["function_call"])) == 0 {
		messages = append(messages, encode(map[string]string{"role": "user", "content": "Continue from the last assistant message."}))
	}
	control := encode(map[string]string{
		"role":    "system",
		"content": "When ready to deliver your final answer, call " + name + " with the complete answer in content. Use the other tools normally when needed. Avoid duplicating your final answer in normal message content.",
	})
	body["messages"] = encode(append([]json.RawMessage{control}, messages...))
	return encode(body), &Request{name: name, cfg: cfg}
}
