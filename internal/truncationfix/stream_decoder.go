package truncationfix

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

var contentPrefix = regexp.MustCompile("^\\s*\\{\\s*\"content\"\\s*:\\s*\"")

type arguments struct {
	raw                      strings.Builder
	pending                  string
	scanned                  int
	started, ended, overflow bool
	emitted                  int
}

func (a *arguments) feed(fragment string, limit int) string {
	if a.overflow || fragment == "" {
		return ""
	}
	if len(fragment) > limit-a.raw.Len() {
		fragment = fragment[:limit-a.raw.Len()]
		a.overflow = true
	}
	a.raw.WriteString(fragment)
	if a.ended {
		return ""
	}
	if !a.started {
		// Canonical arguments start with the single property in the tool schema.
		// Defer alternate shapes to the bounded full JSON parser at finish.
		if a.raw.Len() >= 256 && a.scanned >= 256 {
			return ""
		}
		a.scanned = a.raw.Len()
		match := contentPrefix.FindStringIndex(a.raw.String())
		if match == nil {
			return ""
		}
		a.started = true
		fragment = a.raw.String()[match[1]:]
	}
	data := a.pending + fragment
	a.pending = ""
	var out strings.Builder
	for len(data) > 0 {
		size := 1
		switch data[0] {
		case '"':
			a.ended = true
			data = ""
			continue
		case '\\':
			size = completeEscape(data)
			if size == 0 {
				a.pending = data
				break
			}
			var decoded string
			if json.Unmarshal([]byte("\""+data[:size]+"\""), &decoded) != nil {
				a.ended = true
				data = ""
				continue
			}
			out.WriteString(decoded)
		default:
			if !utf8.FullRuneInString(data) {
				size = 0
				a.pending = data
				break
			}
			_, size = utf8.DecodeRuneInString(data)
			out.WriteString(data[:size])
		}
		if size == 0 {
			break
		}
		data = data[size:]
	}
	a.emitted += out.Len()
	return out.String()
}

func completeEscape(s string) int {
	if len(s) < 2 {
		return 0
	}
	if s[1] != 'u' {
		return 2
	}
	if len(s) < 6 {
		return 0
	}
	hex := strings.ToLower(s[2:6])
	if hex[0] != 'd' || !strings.ContainsRune("89ab", rune(hex[1])) {
		return 6
	}
	// Keep a high surrogate until its possible low surrogate arrives.
	if len(s) == 6 || (len(s) == 7 && s[6] == '\\') {
		return 0
	}
	if strings.HasPrefix(s[6:], "\\u") {
		if len(s) < 12 {
			return 0
		}
		low := strings.ToLower(s[8:12])
		if low[0] == 'd' && strings.ContainsRune("cdef", rune(low[1])) {
			return 12
		}
	}
	return 6
}

func extractContent(raw string, repairLevel string) (string, bool) {
	if repairLevel != "conservative" {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "\x60\x60\x60") {
			if _, rest, found := strings.Cut(trimmed, "\n"); found {
				raw = strings.TrimSpace(strings.TrimSuffix(rest, "\x60\x60\x60"))
			}
		}
	}
	var value struct {
		Content *string
	}
	if json.Unmarshal([]byte(raw), &value) != nil || value.Content == nil {
		return "", false
	}
	return *value.Content, true
}

func (a *arguments) finish(repairLevel string) (string, bool) {
	full, ok := extractContent(a.raw.String(), repairLevel)
	if !ok {
		return "", false
	}
	if a.emitted > len(full) {
		return "", false
	}
	return full[a.emitted:], !a.overflow
}
