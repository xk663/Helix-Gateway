package truncationfix

import (
	"slices"
	"strings"
	"unicode/utf8"
)

const overlapWindow = 512
const minOverlap = 8

func overlapTail(text string) []rune {
	tail := make([]rune, 0, overlapWindow)
	for len(text) > 0 && len(tail) < overlapWindow {
		r, size := utf8.DecodeLastRuneInString(text)
		tail = append(tail, r)
		text = text[:len(text)-size]
	}
	slices.Reverse(tail)
	return tail
}

// overlapCut returns a byte offset and whether a longer match is still possible.
func overlapCut(tail []rune, text string, final bool) (int, bool) {
	if len(tail) < minOverlap || text == "" {
		return 0, false
	}
	var head [overlapWindow]rune
	var ends [overlapWindow + 1]int
	n, offset := 0, 0
	for offset < len(text) && n < len(tail) {
		r, size := utf8.DecodeRuneInString(text[offset:])
		head[n] = r
		n++
		offset += size
		ends[n] = offset
	}
	// Prefix links let us scan the tail once even when it is repetitive.
	var fallback [overlapWindow]int
	for i, matched := 1, 0; i < n; i++ {
		for matched > 0 && head[matched] != head[i] {
			matched = fallback[matched-1]
		}
		if head[matched] == head[i] {
			matched++
		}
		fallback[i] = matched
	}
	matched := 0
	for i, r := range tail {
		for matched > 0 && head[matched] != r {
			matched = fallback[matched-1]
		}
		if head[matched] == r {
			matched++
		}
		if matched == n && i != len(tail)-1 {
			if !final && len(tail)-i-1+n >= minOverlap {
				return 0, true
			}
			matched = fallback[matched-1]
		}
	}
	if matched >= minOverlap {
		return ends[matched], false
	}
	return 0, false
}

// TextOverlap merges the initial fragment of a second stream with existing text.
type TextOverlap struct {
	plain       string
	tail        []rune
	pending     []byte
	initialized bool
	prefix      bool
	done        bool
}

// Merge delays a still-possible full-prefix match until the text either
// diverges or ends. Each new fragment is checked against that prefix once.
func (o *TextOverlap) Merge(plain, text string, final bool) (string, int) {
	if o.done || text == "" && len(o.pending) == 0 {
		return text, 0
	}
	if !o.initialized {
		o.plain = plain
		o.prefix = longEnough(plain)
		o.initialized = true
	}
	offset := len(o.pending)
	if o.prefix {
		n := min(len(text), len(o.plain)-offset)
		o.prefix = strings.HasPrefix(o.plain[offset:], text[:n])
	}
	if offset == 0 && !o.prefix {
		// The common case needs no pending-buffer allocation.
		o.tail = overlapTail(o.plain)
		cut, wait := overlapCut(o.tail, text, final)
		if !wait {
			*o = TextOverlap{done: true}
			return text[cut:], cut
		}
		o.pending = append(o.pending, text...)
		return "", 0
	}
	o.pending = append(o.pending, text...)
	cut := 0
	switch {
	case o.prefix && len(o.pending) >= len(o.plain):
		cut = len(o.plain)
	case o.prefix && !final:
		return "", 0
	case o.prefix && longEnough(string(o.pending)):
		cut = len(o.pending)
	default:
		if o.tail == nil {
			o.tail = overlapTail(o.plain)
		}
		var wait bool
		cut, wait = overlapCut(o.tail, string(o.pending), final)
		if wait {
			return "", 0
		}
	}
	result := string(o.pending[cut:])
	*o = TextOverlap{done: true}
	return result, cut
}

func longEnough(text string) bool {
	// A range loop avoids scanning long replies just to check the threshold.
	count := 0
	for range text {
		count++
		if count == minOverlap {
			return true
		}
	}
	return false
}

func (s *choiceState) mergeSynthetic(text string, final bool, report *Report) string {
	var plain string
	if !s.overlap.initialized && !s.tooLong {
		plain = s.content.String()
	}
	text, cut := s.overlap.Merge(plain, text, final)
	report.DeduplicatedBytes += uint64(cut)
	report.RecoveredBytes += uint64(len(text))
	return text
}
