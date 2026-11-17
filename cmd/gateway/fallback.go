package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	toolErrorLimit = 64 << 10
	retryCacheTTL  = 2 * time.Minute
	retryCacheSize = 256
)

const toolCapability = "(?:tools|tool[- _]calling|function[- _]call(?:ing)?)"

var (
	capabilityDisabled    = regexp.MustCompile("(?i)\\b(?:does not support|doesn't support|do not support)\\s+(?:the\\s+)?" + toolCapability + "\\b|\\b" + toolCapability + "\\s+(?:(?:is|are)\\s+)?(?:not supported|unsupported|not available)\\b")
	toolsParameterUnknown = regexp.MustCompile("(?i)\\b(?:unrecognized|unknown|unsupported)\\s+(?:request\\s+)?(?:argument|parameter|field|capability)(?:\\s+(?:supplied|received))?\\s*[:=]?\\s*(?:['\u0022]tools['\u0022]|tools(?:[;,\\s]|\\.?$))")
	toolChoiceParameter   = regexp.MustCompile("(?i)\\btool[_ -]choice\\b")
)

type retryKey [sha256.Size]byte

func makeRetryKey(req *http.Request, original []byte) retryKey {
	hash := sha256.New()
	_, _ = io.WriteString(hash, req.URL.RequestURI())
	_, _ = hash.Write([]byte{0})
	_ = req.Header.Write(hash)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(original)
	var key retryKey
	hash.Sum(key[:0])
	return key
}

type toolRetryCache struct {
	mu      sync.Mutex
	entries map[retryKey]time.Time
}

func (c *toolRetryCache) contains(key retryKey, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Before(c.entries[key]) {
		return true
	}
	delete(c.entries, key)
	return false
}

func (c *toolRetryCache) remember(key retryKey, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= retryCacheSize {
		var oldest retryKey
		var deadline time.Time
		for entry, expiry := range c.entries {
			if !now.Before(expiry) {
				delete(c.entries, entry)
				continue
			}
			if deadline.IsZero() || expiry.Before(deadline) {
				oldest, deadline = entry, expiry
			}
		}
		if len(c.entries) >= retryCacheSize {
			delete(c.entries, oldest)
		}
	}
	c.entries[key] = now.Add(retryCacheTTL)
}

type toolFallbackTransport struct {
	upstream http.RoundTripper
	cache    *toolRetryCache
}

func (t toolFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.upstream.RoundTrip(req)
	state := requestState(req)
	if state == nil || !state.fix.Synthetic() {
		return resp, err
	}
	defer func() {
		state.original = nil
		state.acceptEncoding = nil
	}()
	if err != nil || state.retried ||
		resp.StatusCode != http.StatusBadRequest || req.Context().Err() != nil {
		return resp, err
	}
	if !toolRejection(resp) || req.Context().Err() != nil {
		return resp, nil
	}
	_ = resp.Body.Close()
	state.retried = true
	state.fix.Note("upstream_rejected_tools")

	retry := req.Clone(req.Context())
	retry.Body = io.NopCloser(bytes.NewReader(state.original))
	retry.GetBody = nil
	retry.ContentLength = int64(len(state.original))
	retry.TransferEncoding = nil
	retry.Header.Del("Content-Length")
	retry.Header.Del("Accept-Encoding")
	for _, value := range state.acceptEncoding {
		retry.Header.Add("Accept-Encoding", value)
	}
	resp, err = t.upstream.RoundTrip(retry)
	if err == nil && resp.StatusCode == http.StatusBadRequest && t.cache != nil {
		t.cache.remember(state.retryKey, time.Now())
	}
	return resp, err
}

type replayBody struct {
	io.Reader
	io.Closer
}

// toolRejection probes bounded wire bytes and restores them along with the
// original closer. Decompression operates on the copy, not the upstream body.
func toolRejection(resp *http.Response) bool {
	encoding := strings.ToLower(strings.TrimSpace(strings.Join(resp.Header.Values("Content-Encoding"), ",")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		return false
	}
	original := resp.Body
	wire, err := io.ReadAll(io.LimitReader(original, toolErrorLimit+1))
	resp.Body = replayBody{Reader: io.MultiReader(bytes.NewReader(wire), original), Closer: original}
	if err != nil || len(wire) > toolErrorLimit {
		return false
	}
	body := wire
	if encoding == "gzip" {
		decoded, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			return false
		}
		defer decoded.Close()
		body, err = io.ReadAll(io.LimitReader(decoded, toolErrorLimit+1))
		if err != nil || len(body) > toolErrorLimit {
			return false
		}
	}
	return explicitToolRejection(body)
}

func explicitToolRejection(body []byte) bool {
	if !json.Valid(body) {
		return messageRejectsTools(string(body))
	}
	type detail struct {
		Message string
		Code    json.RawMessage
		Param   string
	}
	var envelope struct {
		detail
		Error json.RawMessage
	}
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	if len(envelope.Error) > 0 {
		var message string
		if json.Unmarshal(envelope.Error, &message) == nil {
			envelope.Message = message
		} else {
			var nested detail
			if json.Unmarshal(envelope.Error, &nested) != nil {
				return false
			}
			envelope.detail = nested
		}
	}

	param := strings.ToLower(strings.TrimSpace(envelope.Param))
	if param != "" && param != "tools" {
		return false
	}
	var code string
	_ = json.Unmarshal(envelope.Code, &code)
	switch strings.ToLower(code) {
	case "unsupported_tools", "tools_not_supported", "tool_calling_not_supported",
		"unsupported_tool_calling", "unsupported_function_calling", "function_calling_not_supported":
		return true
	case "unsupported_parameter", "unknown_parameter", "unrecognized_parameter", "unsupported_capability":
		if param == "tools" {
			return true
		}
	case "invalid_function_arguments", "invalid_tool_arguments", "tool_not_found", "function_not_found", "invalid_tool_choice":
		return false
	}
	return messageRejectsTools(envelope.Message)
}

func messageRejectsTools(message string) bool {
	return !toolChoiceParameter.MatchString(message) &&
		(capabilityDisabled.MatchString(message) || toolsParameterUnknown.MatchString(message))
}
