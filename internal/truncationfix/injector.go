package truncationfix

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ToolName is stable so consecutive requests can reuse their provider's prompt prefix.
const ToolName = "agw_emit_response"

const constraint = "For your final answer, call agw_emit_response with the complete answer in content. Use other tools normally when needed. Avoid repeating the final answer in ordinary message content."

var transportTool = encode(map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        ToolName,
		"description": "Deliver the final answer to the user as a string.",
		"parameters": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"content": map[string]string{"type": "string"}},
			"required":             []string{"content"},
			"additionalProperties": false,
		},
	},
})

type object map[string]json.RawMessage

func asObject(raw json.RawMessage) object {
	var value object
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

func asArray(raw json.RawMessage) []json.RawMessage {
	var value []json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
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
	name    string
	cfg     Config
	genuine map[string]bool
	report  Report
}

type Report struct {
	RecoveredBytes uint64
	UpstreamLength uint64
	GatewayLength  uint64
	GuardTrips     uint64
	Repairs        uint64
	MissingDone    uint64
	Reasons        map[string]uint64
}

func (r *Request) BufferSize() int { return r.cfg.StreamBufferSize }
func (r *Request) Synthetic() bool { return r.name != "" }
func (r *Request) Report() Report  { return r.report }

func (r *Request) Note(reason string) {
	r.report.note(reason)
}

func (r *Request) Guard(reason string) {
	r.report.guard(reason)
}

func (report *Report) guard(reason string) {
	report.GuardTrips++
	report.note(reason)
}

func (report *Report) note(reason string) {
	if report.Reasons == nil {
		report.Reasons = make(map[string]uint64)
	}
	report.Reasons[reason]++
}

func (r *Request) Record(report Report) {
	r.report.RecoveredBytes += report.RecoveredBytes
	r.report.UpstreamLength += report.UpstreamLength
	r.report.GatewayLength += report.GatewayLength
	r.report.GuardTrips += report.GuardTrips
	r.report.Repairs += report.Repairs
	r.report.MissingDone += report.MissingDone
	if len(report.Reasons) > 0 && r.report.Reasons == nil {
		r.report.Reasons = make(map[string]uint64)
	}
	for reason, count := range report.Reasons {
		r.report.Reasons[reason] += count
	}
}

func hasValue(raw []byte) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// Prepare returns the request body, response processor, and a reason for bypassing it.
func (cfg Config) Prepare(raw []byte) ([]byte, *Request, string) {
	if !cfg.Enabled {
		return raw, nil, "disabled"
	}
	if cfg.AssumeNoTools && cfg.WrapperMode != "repair-only" {
		return raw, nil, "assume_no_tools"
	}
	body := asObject(raw)
	model := asString(body["model"])
	messages := asArray(body["messages"])
	if model == "" || len(messages) == 0 {
		return raw, nil, "invalid_chat_request"
	}
	if !cfg.wraps(model) {
		return raw, nil, "model_excluded"
	}
	if cfg.WrapperMode == "repair-only" {
		return raw, &Request{cfg: cfg}, ""
	}
	if choice := body["tool_choice"]; hasValue(choice) && asString(choice) != "auto" {
		return raw, nil, "explicit_tool_choice"
	}
	for _, key := range []string{"functions", "function_call", "response_format", "audio"} {
		if hasValue(body[key]) && !(key == "response_format" && asString(asObject(body[key])["type"]) == "text") {
			return raw, nil, "incompatible_" + key
		}
	}
	tools := asArray(body["tools"])
	if tools == nil && hasValue(body["tools"]) {
		return raw, nil, "invalid_tools"
	}
	genuine := make(map[string]bool, len(tools))
	for _, tool := range tools {
		definition := asObject(tool)
		name := asString(asObject(definition["function"])["name"])
		if name == "" {
			name = asString(asObject(definition["custom"])["name"])
		}
		if name == ToolName {
			return raw, nil, "tool_name_collision"
		}
		if name != "" {
			genuine[name] = true
		}
	}
	messages, ok := constrainMessages(messages, model)
	if !ok {
		return raw, nil, "pending_tool_turn"
	}
	body["tools"] = encode(append(tools, transportTool))
	body["tool_choice"] = encode("auto")
	body["messages"] = encode(messages)
	return encode(body), &Request{name: ToolName, cfg: cfg, genuine: genuine}, ""
}

func constrainMessages(messages []json.RawMessage, model string) ([]json.RawMessage, bool) {
	last := asObject(messages[len(messages)-1])
	lastRole := asString(last["role"])
	short := strings.ToLower(model[strings.LastIndexByte(model, '/')+1:])
	if lastRole == "assistant" && (len(asArray(last["tool_calls"])) > 0 || hasValue(last["function_call"])) {
		return messages, false
	}
	// Gemini requires a user turn after an assistant prefill.
	needsContinuation := lastRole == "assistant" && strings.HasPrefix(strings.TrimPrefix(short, "agy-"), "gemini-")
	for i, raw := range messages {
		message := asObject(raw)
		if asString(message["role"]) == "system" && appendConstraint(message) {
			messages[i] = encode(message)
			if needsContinuation {
				return appendUser(messages, "Continue from the last assistant message."), true
			}
			return messages, true
		}
	}
	if needsContinuation {
		for i := len(messages) - 2; i >= 0; i-- {
			message := asObject(messages[i])
			if asString(message["role"]) == "user" && appendConstraint(message) {
				messages[i] = encode(message)
				break
			}
		}
		return appendUser(messages, "Continue from the last assistant message."), true
	}
	if lastRole == "user" && appendConstraint(last) {
		messages[len(messages)-1] = encode(last)
		return messages, true
	}
	return appendUser(messages, constraint), true
}

func appendUser(messages []json.RawMessage, text string) []json.RawMessage {
	return append(messages, encode(map[string]string{"role": "user", "content": text}))
}

func appendConstraint(message object) bool {
	var text *string
	if json.Unmarshal(message["content"], &text) == nil && text != nil {
		message["content"] = encode(*text + "\n\n" + constraint)
		return true
	}
	if parts := asArray(message["content"]); parts != nil {
		message["content"] = encode(append(parts, encode(map[string]string{"type": "text", "text": constraint})))
		return true
	}
	return false
}
