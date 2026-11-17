package truncationfix

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// Complete restores an ordinary assistant message from a non-streaming response.
func (r *Request) Complete(raw []byte) []byte {
	if r == nil || !bytes.Contains(raw, []byte(r.name)) {
		return raw
	}
	root := asObject(raw)
	choices := asArray(root["choices"])
	changed := false
	for index, value := range choices {
		choice := asObject(value)
		message := asObject(choice["message"])
		var kept []json.RawMessage
		var recovered strings.Builder
		found, incomplete := false, false
		for _, tool := range asArray(message["tool_calls"]) {
			fn := asObject(asObject(tool)["function"])
			if asString(fn["name"]) != r.name {
				kept = append(kept, tool)
				continue
			}
			found = true
			fragment := readArgument(fn["arguments"])
			text, complete := "", false
			if len(fragment) <= r.cfg.MaxSyntheticToolLength {
				text, complete = extractContent(fragment, r.cfg.RepairLevel)
			}
			if !complete {
				var arg arguments
				text = arg.feed(fragment, r.cfg.MaxSyntheticToolLength)
			}
			if recovered.Len()+len(text) <= r.cfg.MaxSyntheticToolLength {
				recovered.WriteString(text)
			} else {
				complete = false
			}
			incomplete = incomplete || !complete
		}
		if !found {
			continue
		}
		content := asString(message["content"])
		if recovered.Len() > 0 {
			content = recovered.String()
		}
		message["content"] = encode(content + repairSuffix(content, r.cfg.RepairLevel))
		setCalls(message, kept)
		rewriteFinish(choice, len(kept) > 0, incomplete)
		choice["message"] = encode(message)
		choices[index] = encode(choice)
		changed = true
	}
	if !changed {
		return raw
	}
	root["choices"] = encode(choices)
	return encode(root)
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
		return
	}
	message["tool_calls"] = encode(genuine)
}

func rewriteFinish(choice object, genuine, incomplete bool) {
	reason := asString(choice["finish_reason"])
	switch reason {
	case "tool_calls", "function_call", "stop":
		if incomplete {
			choice["finish_reason"] = encode("length")
		} else if !genuine && reason != "stop" {
			choice["finish_reason"] = encode("stop")
		}
	}
}

// Stream is request-local and consumes already framed OpenAI JSON chunks.
type Stream struct {
	request *Request
	choices map[int64]*choiceState
}

type choiceState struct {
	synthetic         map[int64]*arguments
	genuine           map[int64]int64
	content           strings.Builder
	tooLong, finished bool
}

func (r *Request) Stream() *Stream {
	if r == nil {
		return nil
	}
	return &Stream{request: r, choices: make(map[int64]*choiceState)}
}

func number(raw json.RawMessage) int64 {
	value, _ := strconv.ParseInt(string(raw), 10, 64)
	return value
}

func (s *Stream) Chunk(raw []byte) []byte {
	if s == nil || (len(s.choices) == 0 && !bytes.Contains(raw, []byte("\"tool_calls\""))) {
		return raw
	}
	root := asObject(raw)
	choices := asArray(root["choices"])
	changed := false
	for index, value := range choices {
		choice := asObject(value)
		delta := asObject(choice["delta"])
		if delta == nil {
			continue
		}
		key := number(choice["index"])
		state := s.choices[key]
		calls := asArray(delta["tool_calls"])
		if state == nil {
			if len(calls) == 0 {
				continue
			}
			state = &choiceState{synthetic: make(map[int64]*arguments), genuine: make(map[int64]int64)}
			s.choices[key] = state
		}
		var kept []json.RawMessage
		var text strings.Builder
		choiceChanged := false
		for _, tool := range calls {
			call := asObject(tool)
			fn := asObject(call["function"])
			idx := number(call["index"])
			arg := state.synthetic[idx]
			if arg == nil && asString(fn["name"]) == s.request.name {
				arg = &arguments{}
				state.synthetic[idx] = arg
			}
			if arg != nil {
				text.WriteString(arg.feed(readArgument(fn["arguments"]), s.request.cfg.MaxSyntheticToolLength))
				choiceChanged = true
				continue
			}
			// OpenAI tool indices are zero-based. Keep genuine calls contiguous
			// when a synthetic call is removed ahead of them.
			resolved := state.genuineIndex(idx)
			if resolved != idx {
				call["index"] = encode(resolved)
				tool = encode(call)
				choiceChanged = true
			}
			kept = append(kept, tool)
		}
		if choiceChanged {
			setCalls(delta, kept)
		}
		if text.Len() > 0 {
			state.record(text.String(), s.request.cfg)
		}
		if asString(choice["finish_reason"]) != "" && len(state.synthetic) > 0 && !state.finished {
			incomplete := false
			indices := make([]int64, 0, len(state.synthetic))
			for idx := range state.synthetic {
				indices = append(indices, idx)
			}
			slices.Sort(indices)
			for _, idx := range indices {
				arg := state.synthetic[idx]
				tail, complete := arg.finish(s.request.cfg.RepairLevel)
				text.WriteString(tail)
				state.record(tail, s.request.cfg)
				incomplete = incomplete || !complete
			}
			if !state.tooLong {
				text.WriteString(repairSuffix(state.content.String(), s.request.cfg.RepairLevel))
			}
			state.finished = true
			rewriteFinish(choice, len(state.genuine) > 0, incomplete)
			choiceChanged = true
		}
		if text.Len() > 0 {
			delta["content"] = encode(asString(delta["content"]) + text.String())
		}
		if choiceChanged {
			choice["delta"] = encode(delta)
			choices[index] = encode(choice)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	root["choices"] = encode(choices)
	return encode(root)
}

func (s *choiceState) genuineIndex(index int64) int64 {
	if resolved, exists := s.genuine[index]; exists {
		return resolved
	}
	resolved := index
	for syntheticIndex := range s.synthetic {
		if syntheticIndex < index {
			resolved--
		}
	}
	for _, assigned := range s.genuine {
		if assigned == resolved {
			resolved = index
			break
		}
	}
	s.genuine[index] = resolved
	return resolved
}

func (s *choiceState) record(fragment string, cfg Config) {
	if fragment == "" {
		return
	}
	if s.tooLong || len(fragment) > cfg.MaxSyntheticToolLength-s.content.Len() {
		s.tooLong = true
		return
	}
	if s.content.Cap() == 0 {
		s.content.Grow(min(cfg.StreamBufferSize, cfg.MaxSyntheticToolLength))
	}
	s.content.WriteString(fragment)
}
