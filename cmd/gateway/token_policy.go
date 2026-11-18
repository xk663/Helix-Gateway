package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const defaultTokenCeiling = 8192
const policyEntries = 256

type chatParameters struct {
	Model               string          `json:"model"`
	MaxTokens           json.RawMessage `json:"max_tokens"`
	MaxCompletionTokens json.RawMessage `json:"max_completion_tokens"`
}

func (p chatParameters) cap() (string, int) {
	var field string
	var raw json.RawMessage
	switch {
	case len(p.MaxTokens) != 0 && len(p.MaxCompletionTokens) == 0:
		field, raw = "max_tokens", p.MaxTokens
	case len(p.MaxCompletionTokens) != 0 && len(p.MaxTokens) == 0:
		field, raw = "max_completion_tokens", p.MaxCompletionTokens
	}
	value, _ := strconv.Atoi(string(raw))
	if value <= 0 {
		return "", 0
	}
	return field, value
}

func configuredTokenCeiling() int {
	value := strings.TrimSpace(os.Getenv("TRUNCATIONFIX_MAX_TOKENS_CEILING"))
	if strings.EqualFold(value, "off") || value == "0" {
		return 0
	}
	if ceiling, err := strconv.Atoi(value); err == nil && ceiling > 0 {
		return ceiling
	}
	return defaultTokenCeiling
}

type tokenPolicy struct {
	mu      sync.Mutex
	models  map[retryKey]tokenPolicyEntry
	ceiling int
}

type tokenPolicyEntry struct {
	next      int
	limit     int
	streak    int
	streakCap int
}

// Preserve caller budgets already above the configured ceiling. The ceiling
// caps gateway increases, while a learned hard limit can cap later requests.
func (p *tokenPolicy) cap(key retryKey, original int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.models[key]
	budget := max(original, entry.next)
	if entry.limit > 0 {
		budget = min(budget, entry.limit)
	}
	return budget
}

func (p *tokenPolicy) set(key retryKey, entry tokenPolicyEntry) {
	if _, exists := p.models[key]; !exists && len(p.models) >= policyEntries {
		for old := range p.models {
			delete(p.models, old)
			break
		}
	}
	p.models[key] = entry
}

func (p *tokenPolicy) observe(key retryKey, sent int, hit bool) {
	if p.ceiling == 0 || sent <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.models[key]
	if !hit {
		if entry.streak != 0 {
			entry.streak = 0
			p.set(key, entry)
		}
		return
	}
	if entry.streakCap != sent {
		entry.streak, entry.streakCap = 0, sent
	}
	entry.streak++
	if entry.streak == 2 {
		ceiling := p.ceiling
		if entry.limit > 0 {
			ceiling = min(ceiling, entry.limit)
		}
		if sent < ceiling {
			entry.next = max(entry.next, sent+min(sent, ceiling-sent))
		}
		entry.streak = 0
	}
	p.set(key, entry)
}

var outputLimit = regexp.MustCompile("(?i)\\bmax(?:_completion)?_tokens\\b[^\\n]{0,80}(?:<=|≤|at most|maximum of|must not exceed)\\s*(\\d+)")
var budgetField = regexp.MustCompile("(?i)\\b(?:max(?:_completion)?_tokens|context length|context window)\\b")
var budgetRejection = regexp.MustCompile("(?i)(maximum|exceed|too (?:large|long|high)|must be|at most|out of range|context_length_exceeded)")

func (p *tokenPolicy) reject(key retryKey, original, sent int, detail rejectionDetail) {
	if p.ceiling == 0 || sent <= 1 {
		return
	}
	description := detail.Param + " " + detail.Message
	if !(budgetField.MatchString(description) && budgetRejection.MatchString(description)) &&
		detail.code() != "context_length_exceeded" {
		return
	}

	hardLimit := 0
	if match := outputLimit.FindStringSubmatch(detail.Message); len(match) > 1 {
		hardLimit, _ = strconv.Atoi(match[1])
	}
	if hardLimit >= sent {
		hardLimit = 0
	}
	// A context window is shared with the input. Revert the gateway's bump
	// without treating a prompt-dependent error as the model's output limit.
	if hardLimit == 0 && sent <= original {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.models[key]
	if hardLimit > 0 {
		entry.limit = hardLimit
		entry.next = min(entry.next, hardLimit)
	} else if entry.next >= sent {
		entry.next = max(original, sent-sent/4)
		if entry.next >= sent {
			entry.next = sent - 1
		}
	}
	entry.streak = 0
	p.set(key, entry)
}

func rewriteTokenCap(body []byte, field string, cap int) []byte {
	return replaceChatField(body, field, json.RawMessage(strconv.Itoa(cap)))
}

func replaceChatField(body []byte, field string, value json.RawMessage) []byte {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return body
	}
	object[field] = value
	result, err := json.Marshal(object)
	if err != nil {
		return body
	}
	return result
}
