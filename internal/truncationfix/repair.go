package truncationfix

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"strings"
)

func repairSuffix(content, level string) string {
	text := strings.TrimSpace(content)
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		if suffix := closeJSON(content); suffix != "" {
			return suffix
		}
	}
	if level == "conservative" {
		return ""
	}
	if fence := openFence(content); fence != "" {
		return "\n" + fence
	}
	if level == "aggressive" && strings.HasPrefix(text, "<") {
		return closeXML(text)
	}
	return ""
}

// closeJSON appends only closing tokens, and only if they make the full JSON valid.
func closeJSON(text string) string {
	if json.Valid([]byte(text)) {
		return ""
	}
	var stack []byte
	inString, escaped := false, false
	for _, b := range []byte(text) {
		if inString {
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != b {
				return ""
			}
			stack = stack[:len(stack)-1]
		}
	}
	if escaped {
		return ""
	}
	var suffix strings.Builder
	if inString {
		suffix.WriteByte('"')
	}
	for i := len(stack) - 1; i >= 0; i-- {
		suffix.WriteByte(stack[i])
	}
	if suffix.Len() > 0 && json.Valid([]byte(text+suffix.String())) {
		return suffix.String()
	}
	return ""
}

func openFence(text string) string {
	var fence string
	for line := range strings.SplitSeq(text, "\n") {
		marker, rest := fencePrefix(line)
		if marker == "" {
			continue
		}
		if fence == "" {
			fence = marker
		} else if strings.HasPrefix(marker, fence) && strings.TrimSpace(rest) == "" {
			fence = ""
		}
	}
	return fence
}

func stripFence(raw string) string {
	header, body, hasLine := strings.Cut(strings.TrimSpace(raw), "\n")
	open, _ := fencePrefix(header)
	if !hasLine || open == "" {
		return raw
	}
	body = strings.TrimRight(body, " \t\r\n")
	if pos := strings.LastIndexByte(body, '\n'); pos >= 0 {
		close, rest := fencePrefix(body[pos+1:])
		if strings.HasPrefix(close, open) && strings.TrimSpace(rest) == "" {
			body = body[:pos]
		}
	}
	return strings.TrimSpace(body)
}

func fencePrefix(line string) (string, string) {
	line = strings.TrimSuffix(line, "\r")
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || (trimmed[0] != 96 && trimmed[0] != '~') {
		return "", ""
	}
	n := 1
	for n < len(trimmed) && trimmed[n] == trimmed[0] {
		n++
	}
	if n < 3 {
		return "", ""
	}
	return trimmed[:n], trimmed[n:]
}

func closeXML(text string) string {
	decoder := xml.NewDecoder(strings.NewReader(text))
	var stack []string
	for {
		token, err := decoder.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ""
		}
		switch value := token.(type) {
		case xml.StartElement:
			stack = append(stack, qualifiedName(value.Name))
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1] != qualifiedName(value.Name) {
				return ""
			}
			stack = stack[:len(stack)-1]
		}
	}
	var suffix strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		suffix.WriteString("</" + stack[i] + ">")
	}
	return suffix.String()
}

func qualifiedName(name xml.Name) string {
	if name.Space == "" {
		return name.Local
	}
	return name.Space + ":" + name.Local
}
