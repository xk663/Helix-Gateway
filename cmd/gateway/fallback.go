package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	toolErrorLimit = 64 << 10
	retryCacheTTL  = 10 * time.Minute
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

func makeModelKey(target *url.URL, req *http.Request, model string) retryKey {
	hash := sha256.New()
	for _, value := range []string{target.String(), model, req.URL.RawQuery} {
		_, _ = io.WriteString(hash, value)
		_, _ = hash.Write([]byte{0})
	}
	for _, name := range []string{"Authorization", "X-API-Key", "Api-Key", "Cookie",
		"OpenAI-Organization", "OpenAI-Project"} {
		_, _ = io.WriteString(hash, name)
		_, _ = hash.Write([]byte{0})
		for _, value := range req.Header.Values(name) {
			_, _ = io.WriteString(hash, value)
			_, _ = hash.Write([]byte{0})
		}
	}
	var key retryKey
	hash.Sum(key[:0])
	return key
}

func forceKey(model retryKey) retryKey {
	return sha256.Sum256(append(model[:], 1))
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
	policy   *tokenPolicy
}

func (t toolFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	state := requestState(req)
	if state != nil {
		state.upstreamCalls++
	}
	resp, err := t.upstream.RoundTrip(req)
	if state == nil || state.fix == nil {
		return resp, err
	}
	defer func() {
		state.original = nil
		state.forcedPayload = nil
		state.acceptEncoding = nil
	}()
	if err != nil ||
		resp.StatusCode != http.StatusBadRequest || req.Context().Err() != nil {
		return resp, err
	}
	detail, ok := parseRejection(readRejectionBody(resp))
	if ok && t.policy != nil {
		t.policy.reject(state.modelKey, state.clientCap, state.sentCap, detail)
	}
	if !state.fix.Synthetic() || !ok || req.Context().Err() != nil {
		return resp, nil
	}
	withoutTools := detail.toolsRejected()
	automaticChoice := !withoutTools && state.fix.ForcedTool() &&
		len(state.forcedPayload) > 0 && detail.forcedChoiceRejected()
	if (!withoutTools && !automaticChoice) || req.Context().Err() != nil {
		return resp, nil
	}
	_ = resp.Body.Close()

	retry := req.Clone(req.Context())
	retryBody := state.original
	if automaticChoice {
		retryBody = replaceChatField(state.forcedPayload, "tool_choice", json.RawMessage(`"auto"`))
		state.fix.Note("upstream_rejected_forced_choice")
		if t.cache != nil {
			t.cache.remember(state.forcedKey, time.Now())
		}
	} else {
		state.passthrough = true
		state.fix.Note("upstream_rejected_tools")
		retry.Header.Del("Accept-Encoding")
		for _, value := range state.acceptEncoding {
			retry.Header.Add("Accept-Encoding", value)
		}
	}
	retry.Body = io.NopCloser(bytes.NewReader(retryBody))
	retry.GetBody = nil
	retry.ContentLength = int64(len(retryBody))
	retry.TransferEncoding = nil
	retry.Header.Del("Content-Length")
	state.upstreamCalls++
	resp, err = t.upstream.RoundTrip(retry)
	if err == nil && t.cache != nil && withoutTools {
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			t.cache.remember(state.modelKey, time.Now())
		case resp.StatusCode == http.StatusBadRequest:
			t.cache.remember(state.retryKey, time.Now())
		}
	}
	return resp, err
}

type replayBody struct {
	io.Reader
	io.Closer
}

// readRejectionBody probes bounded wire bytes and restores them along with the
// original closer. Decompression operates on the copy, not the upstream body.
func readRejectionBody(resp *http.Response) []byte {
	encoding := strings.ToLower(strings.TrimSpace(strings.Join(resp.Header.Values("Content-Encoding"), ",")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		return nil
	}
	original := resp.Body
	wire, err := io.ReadAll(io.LimitReader(original, toolErrorLimit+1))
	resp.Body = replayBody{Reader: io.MultiReader(bytes.NewReader(wire), original), Closer: original}
	if err != nil || len(wire) > toolErrorLimit {
		return nil
	}
	body := wire
	if encoding == "gzip" {
		decoded, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			return nil
		}
		defer decoded.Close()
		body, err = io.ReadAll(io.LimitReader(decoded, toolErrorLimit+1))
		if err != nil || len(body) > toolErrorLimit {
			return nil
		}
	}
	return body
}

func explicitToolRejection(body []byte) bool {
	detail, ok := parseRejection(body)
	return ok && detail.toolsRejected()
}

type rejectionDetail struct {
	Message string
	Code    json.RawMessage
	Param   string
}

func parseRejection(body []byte) (rejectionDetail, bool) {
	var envelope struct {
		rejectionDetail
		Error json.RawMessage
	}
	if json.Unmarshal(body, &envelope) != nil {
		return rejectionDetail{}, false
	}
	if len(envelope.Error) > 0 {
		var message string
		if json.Unmarshal(envelope.Error, &message) == nil {
			envelope.Message = message
		} else {
			var nested rejectionDetail
			if json.Unmarshal(envelope.Error, &nested) != nil {
				return rejectionDetail{}, false
			}
			envelope.rejectionDetail = nested
		}
	}

	return envelope.rejectionDetail, true
}

func (d rejectionDetail) code() string {
	var code string
	_ = json.Unmarshal(d.Code, &code)
	return strings.ToLower(code)
}

func (d rejectionDetail) toolsRejected() bool {
	param := strings.ToLower(strings.TrimSpace(d.Param))
	if param != "" && param != "tools" {
		return false
	}
	switch d.code() {
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
	return messageRejectsTools(d.Message)
}

var refusedChoice = regexp.MustCompile("(?i)\\btool[_ -]choice\\b\\s+(?:is\\s+)?(?:not supported|unsupported)\\b")

func (d rejectionDetail) forcedChoiceRejected() bool {
	param := strings.ToLower(strings.TrimSpace(d.Param))
	if param != "" && param != "tool_choice" {
		return false
	}
	return (param == "tool_choice" && d.code() == "unsupported_parameter") || refusedChoice.MatchString(d.Message)
}

func messageRejectsTools(message string) bool {
	return !toolChoiceParameter.MatchString(message) &&
		(capabilityDisabled.MatchString(message) || toolsParameterUnknown.MatchString(message))
}
