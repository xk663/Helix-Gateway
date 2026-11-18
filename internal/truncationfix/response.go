package truncationfix

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
)

var ErrInvalidResponse = errors.New("upstream response does not follow the chat completion protocol")

// CheckOutput prevents the reserved transport name from escaping via alternate fields.
func (r *Request) CheckOutput(raw []byte) error {
	if !r.Synthetic() {
		return nil
	}
	if !bytes.Contains(raw, []byte(ToolName)) && !bytes.Contains(raw, []byte("\\u")) {
		return nil
	}
	var structure any
	if json.Unmarshal(raw, &structure) != nil || hasReservedTool(structure, false) {
		return ErrInvalidResponse
	}
	return nil
}

func hasReservedTool(value any, toolField bool) bool {
	switch node := value.(type) {
	case string:
		return toolField && node == ToolName
	case map[string]any:
		if name, _ := node["name"].(string); name == ToolName {
			kind, _ := node["type"].(string)
			if toolField || kind == "function" || kind == "tool_use" || kind == "custom" {
				return true
			}
		}
		for key, child := range node {
			switch key {
			case "function", "function_call", "custom", "tool_calls", "tools", "tool_choice":
				if hasReservedTool(child, true) {
					return true
				}
			default:
				if hasReservedTool(child, false) {
					return true
				}
			}
		}
	case []any:
		for _, child := range node {
			if hasReservedTool(child, toolField) {
				return true
			}
		}
	}
	return false
}

// Complete restores transport text and performs append-only repairs.
func (r *Request) Complete(raw []byte) ([]byte, error) {
	root := asObject(raw)
	choices := asArray(root["choices"])
	if len(choices) == 0 || hasValue(root["error"]) {
		return nil, ErrInvalidResponse
	}
	changed := false
	var report Report
	for index, value := range choices {
		choice := asObject(value)
		message := asObject(choice["message"])
		if message == nil {
			return nil, ErrInvalidResponse
		}
		content := asString(message["content"])
		upstreamReason := asString(choice["finish_reason"])
		found, incomplete := false, false
		if r.Synthetic() {
			var kept []json.RawMessage
			var recovered strings.Builder
			for _, tool := range asArray(message["tool_calls"]) {
				fn := asObject(asObject(tool)["function"])
				if asString(fn["name"]) != r.name {
					kept = append(kept, tool)
					continue
				}
				found = true
				fragment := readArgument(fn["arguments"])
				text, complete := "", false
				budget := r.cfg.MaxSyntheticToolLength - recovered.Len()
				if len(fragment) <= budget {
					text, complete = extractContent(fragment, r.cfg.RepairLevel)
				}
				if !complete {
					var arg arguments
					text = arg.feed(fragment, budget)
					report.noteIncomplete(arg.overflow)
				}
				recovered.WriteString(text)
				incomplete = incomplete || !complete
			}
			if found {
				report.ToolHits++
				setCalls(message, kept)
				var overlap TextOverlap
				text, cut := overlap.Merge(content, recovered.String(), true)
				content += text
				report.RecoveredBytes += uint64(len(text))
				report.DeduplicatedBytes += uint64(cut)
				rewriteFinish(choice, len(kept) > 0, incomplete)
				changed = true
			} else {
				report.note("tool_unused")
			}
		}
		reason := asString(choice["finish_reason"])
		if reason == "" {
			choice["finish_reason"] = encode("length")
			reason, changed = "length", true
			report.note("missing_finish_reason")
		}
		report.recordLength(upstreamReason, reason)
		suffix := r.repair(content, &report)
		if found || suffix != "" {
			message["content"] = encode(content + suffix)
			choice["message"] = encode(message)
			changed = true
		}
		choices[index] = encode(choice)
	}
	if changed {
		root["choices"] = encode(choices)
		raw = encode(root)
	}
	if err := r.CheckOutput(raw); err != nil {
		return nil, err
	}
	r.Record(report)
	return raw, nil
}

func (report *Report) noteIncomplete(limit bool) {
	if limit {
		report.guard("tool_arguments_limit")
	} else {
		report.note("incomplete_tool_arguments")
	}
}

func (report *Report) recordLength(upstream, downstream string) {
	if downstream != "length" {
		return
	}
	if upstream == "length" {
		report.UpstreamLength++
	} else {
		report.GatewayLength++
	}
}

func (r *Request) repair(content string, report *Report) string {
	if len(content) > r.cfg.MaxSyntheticToolLength {
		report.guard("repair_buffer_limit")
		return ""
	}
	suffix := repairSuffix(content, r.cfg.RepairLevel)
	if suffix != "" {
		report.Repairs++
	}
	return suffix
}

func readArgument(raw json.RawMessage) string {
	if obj := bytes.TrimSpace(raw); len(obj) > 0 && obj[0] == '{' {
		return string(obj)
	}
	return asString(raw)
}

func setCalls(message object, genuine []json.RawMessage) {
	if len(genuine) == 0 {
		delete(message, "tool_calls")
	} else {
		message["tool_calls"] = encode(genuine)
	}
}

func rewriteFinish(choice object, genuine, incomplete bool) {
	switch reason := asString(choice["finish_reason"]); reason {
	case "", "tool_calls", "function_call", "stop":
		if incomplete {
			choice["finish_reason"] = encode("length")
		} else if !genuine && reason != "" {
			choice["finish_reason"] = encode("stop")
		}
	}
}

// Stream consumes complete SSE data payloads and keeps independent choice state.
type Stream struct {
	request     *Request
	choices     map[int64]*choiceState
	base        object
	deferRepair bool
}

type choiceState struct {
	synthetic map[int64]*arguments
	genuine   map[int64]int64
	content   strings.Builder
	overlap   TextOverlap
	tooLong   bool
	finished  bool
}

func (r *Request) Stream() *Stream {
	return &Stream{request: r, choices: make(map[int64]*choiceState)}
}

func (r *Request) RepairStream() *Stream {
	return (&Request{cfg: r.cfg}).Stream()
}

func (s *Stream) DeferRepair(deferRepair bool) { s.deferRepair = deferRepair }

func (r *Request) RepairText(text string) (string, Report) {
	var report Report
	return r.repair(text, &report), report
}

func (r *Request) HasClientTools() bool { return len(r.genuine) != 0 }

func number(raw json.RawMessage) int64 {
	value, _ := strconv.ParseInt(string(raw), 10, 64)
	return value
}

func (s *Stream) Chunk(raw []byte) ([]byte, error) {
	root := asObject(raw)
	choices := asArray(root["choices"])
	if root == nil || choices == nil || hasValue(root["error"]) {
		return nil, ErrInvalidResponse
	}
	var report Report
	changed := false
	visible := choices[:0]
	for _, value := range choices {
		choice := asObject(value)
		if choice == nil || (len(choices) > 1 && !hasValue(choice["index"])) {
			return nil, ErrInvalidResponse
		}
		key := number(choice["index"])
		state := s.choices[key]
		if state == nil {
			if len(s.choices) >= 128 {
				return nil, ErrInvalidResponse
			}
			state = &choiceState{}
			s.choices[key] = state
		}
		delta := asObject(choice["delta"])
		reason := asString(choice["finish_reason"])
		if delta == nil {
			if reason == "" {
				return nil, ErrInvalidResponse
			}
			delta = object{}
		}
		text, modified, err := s.transformCalls(state, delta)
		if err != nil {
			return nil, err
		}
		plain := asString(delta["content"])
		var before string
		if !state.finished {
			if plain != "" && len(state.overlap.pending) != 0 {
				before = state.mergeSynthetic("", true, &report)
				state.record(before, s.request.cfg)
			}
			state.record(plain, s.request.cfg)
			text = state.mergeSynthetic(text, false, &report)
			state.record(text, s.request.cfg)
		}
		if reason != "" && !state.finished {
			extra, incomplete := s.finishChoice(state, &report)
			if len(state.synthetic) > 0 {
				rewriteFinish(choice, len(state.genuine) > 0, incomplete)
				modified = true
			}
			report.recordLength(reason, asString(choice["finish_reason"]))
			text += extra
		}
		if text != "" || before != "" {
			delta["content"] = encode(before + plain + text)
			modified = true
		}
		if modified {
			changed = true
			if len(delta) == 0 && reason == "" {
				continue
			}
			choice["delta"] = encode(delta)
			value = encode(choice)
		}
		visible = append(visible, value)
	}
	if changed {
		root["choices"] = encode(visible)
		raw = encode(root)
	}
	if err := s.request.CheckOutput(raw); err != nil {
		return nil, err
	}
	if len(choices) > 0 {
		if s.base == nil {
			s.base = object{}
		}
		for _, key := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
			if hasValue(root[key]) {
				s.base[key] = root[key]
			}
		}
	}
	s.request.Record(report)
	if changed && len(visible) == 0 && !hasValue(root["usage"]) {
		return nil, nil
	}
	return raw, nil
}

func (s *Stream) transformCalls(state *choiceState, delta object) (string, bool, error) {
	if !s.request.Synthetic() {
		return "", false, nil
	}
	calls := asArray(delta["tool_calls"])
	if len(calls) == 0 {
		return "", false, nil
	}
	if state.finished {
		return "", false, ErrInvalidResponse
	}
	if state.synthetic == nil {
		state.synthetic = make(map[int64]*arguments)
		state.genuine = make(map[int64]int64)
	}
	var kept []json.RawMessage
	var text strings.Builder
	modified := false
	for _, tool := range calls {
		call := asObject(tool)
		fn := asObject(call["function"])
		if call == nil || (len(calls) > 1 && !hasValue(call["index"])) {
			return "", false, ErrInvalidResponse
		}
		idx := number(call["index"])
		name := asString(fn["name"])
		if fn == nil {
			name = asString(asObject(call["custom"])["name"])
		}
		arg := state.synthetic[idx]
		_, genuine := state.genuine[idx]
		if arg == nil && fn != nil && name == s.request.name {
			if genuine || len(state.synthetic) >= 4 {
				return "", false, ErrInvalidResponse
			}
			arg = &arguments{}
			state.synthetic[idx] = arg
		}
		if arg != nil {
			if name != "" && name != s.request.name {
				return "", false, ErrInvalidResponse
			}
			text.WriteString(arg.feed(readArgument(fn["arguments"]), s.request.cfg.MaxSyntheticToolLength))
			modified = true
			continue
		}
		// A continuation must refer to a call whose name was already observed.
		if !genuine && (name == "" || (strings.HasPrefix(s.request.name, name) && !s.request.genuine[name])) {
			return "", false, ErrInvalidResponse
		}
		resolved := state.genuineIndex(idx)
		if resolved != idx {
			call["index"] = encode(resolved)
			tool = encode(call)
			modified = true
		}
		kept = append(kept, tool)
	}
	if modified {
		setCalls(delta, kept)
	}
	return text.String(), modified, nil
}

func (s *Stream) finishChoice(state *choiceState, report *Report) (string, bool) {
	var extra strings.Builder
	incomplete := false
	if len(state.synthetic) == 0 && s.request.Synthetic() {
		report.note("tool_unused")
	} else if len(state.synthetic) > 0 {
		report.ToolHits++
	}
	for _, idx := range slices.Sorted(maps.Keys(state.synthetic)) {
		arg := state.synthetic[idx]
		tail, complete := arg.finish(s.request.cfg.RepairLevel)
		extra.WriteString(tail)
		if !complete {
			incomplete = true
			report.noteIncomplete(arg.overflow)
		}
	}
	text := state.mergeSynthetic(extra.String(), true, report)
	state.record(text, s.request.cfg)
	extra.Reset()
	extra.WriteString(text)
	state.finished = true
	if s.deferRepair {
		return extra.String(), incomplete
	}
	if !state.tooLong {
		extra.WriteString(s.request.repair(state.content.String(), report))
	} else {
		report.guard("repair_buffer_limit")
	}
	return extra.String(), incomplete
}

// End builds terminal choices and leaves their metrics for the downstream writer to acknowledge.
func (s *Stream) End(failed bool) ([]byte, Report) {
	var choices []object
	var report Report
	indices := slices.Sorted(maps.Keys(s.choices))
	for _, idx := range indices {
		state := s.choices[idx]
		if state.finished {
			continue
		}
		var extra string
		if !failed {
			extra, _ = s.finishChoice(state, &report)
			report.note("missing_finish_reason")
		}
		state.finished = true
		delta := object{}
		if extra != "" {
			delta["content"] = encode(extra)
		}
		choices = append(choices, object{"index": encode(idx), "delta": encode(delta), "finish_reason": encode("length")})
	}
	if failed && len(choices) == 0 {
		idx := int64(0)
		if len(indices) > 0 {
			idx = indices[0]
		}
		choices = append(choices, object{"index": encode(idx), "delta": encode(object{}), "finish_reason": encode("length")})
	}
	if len(choices) == 0 {
		return nil, report
	}
	report.GatewayLength = uint64(len(choices))
	result := maps.Clone(s.base)
	if result == nil {
		result = object{"object": encode("chat.completion.chunk")}
	}
	result["choices"] = encode(choices)
	return encode(result), report
}

func (s *choiceState) genuineIndex(index int64) int64 {
	if resolved, exists := s.genuine[index]; exists {
		return resolved
	}
	// Synthetic calls never reserve a downstream slot. Assign the next free
	// index on first appearance and retain it through every later fragment.
	resolved := int64(len(s.genuine))
	s.genuine[index] = resolved
	return resolved
}

func (s *choiceState) record(fragment string, cfg Config) {
	if fragment == "" || s.tooLong {
		return
	}
	if len(fragment) > cfg.MaxSyntheticToolLength-s.content.Len() {
		s.tooLong = true
		return
	}
	if s.content.Cap() == 0 {
		s.content.Grow(min(cfg.StreamBufferSize, cfg.MaxSyntheticToolLength))
	}
	s.content.WriteString(fragment)
}
