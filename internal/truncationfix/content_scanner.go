package truncationfix

import "encoding/json"

// contentScanner locates the opening quote of the top-level content value.
// Each byte is visited once, including strings and nested values before it.
type contentScanner struct {
	offset, keyStart, depth     int
	phase                       scanPhase
	contentKey, quoted, escaped bool
}

type scanPhase uint8

const (
	scanObject scanPhase = iota
	scanKeyStart
	scanKey
	scanColon
	scanValue
	scanOther
	scanFinished
)

func (s *contentScanner) contentOffset(raw string) (int, bool) {
	for s.offset < len(raw) && s.phase != scanFinished {
		c := raw[s.offset]
		s.offset++
		switch s.phase {
		case scanObject:
			if c == '{' {
				s.phase = scanKeyStart
			} else if !jsonSpace(c) {
				s.phase = scanFinished
			}
		case scanKeyStart:
			if c == '"' {
				s.keyStart, s.phase = s.offset-1, scanKey
			} else if !jsonSpace(c) {
				s.phase = scanFinished
			}
		case scanKey:
			if s.stringEnd(c) {
				var key string
				s.contentKey = json.Unmarshal([]byte(raw[s.keyStart:s.offset]), &key) == nil && key == "content"
				s.phase = scanColon
			}
		case scanColon:
			if c == ':' {
				s.phase = scanValue
			} else if !jsonSpace(c) {
				s.phase = scanFinished
			}
		case scanValue:
			if jsonSpace(c) {
				continue
			}
			if s.contentKey && c == '"' {
				return s.offset, true
			}
			s.phase = scanOther
			s.skipValue(c)
		case scanOther:
			s.skipValue(c)
		}
	}
	return 0, false
}

func (s *contentScanner) skipValue(c byte) {
	if s.quoted {
		if s.stringEnd(c) {
			s.quoted = false
		}
		return
	}
	switch c {
	case '"':
		s.quoted = true
	case '{', '[':
		s.depth++
	case '}', ']':
		if s.depth > 0 {
			s.depth--
		} else {
			s.phase = scanFinished
		}
	case ',':
		if s.depth == 0 {
			s.phase = scanKeyStart
		}
	}
}

func (s *contentScanner) stringEnd(c byte) bool {
	if s.escaped {
		s.escaped = false
		return false
	}
	if c == '\\' {
		s.escaped = true
		return false
	}
	return c == '"'
}

func jsonSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
