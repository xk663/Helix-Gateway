package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"helix-gateway-local/internal/truncationfix"
)

const (
	maxBody          = 32 << 20
	maxEvent         = 8 << 20
	writeIdleTimeout = 30 * time.Second
)

type requestKey struct{}

type chatState struct {
	fix            *truncationfix.Request
	original       []byte
	acceptEncoding []string
	retryKey       retryKey
	retried        bool
	terminal       *truncationfix.Report
}

type deliveryWriter struct {
	http.ResponseWriter
	failed atomic.Bool
}

func (w *deliveryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *deliveryWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if err != nil || n != len(data) {
		w.failed.Store(true)
	}
	return n, err
}

func (w *deliveryWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil {
		w.failed.Store(true)
	}
	return err
}

func main() {
	upstream := flag.String("upstream", "http://127.0.0.1:8317/v1", "Upstream OpenAI-compatible API base URL")
	port := flag.Int("port", 8318, "Loopback HTTP port")
	flag.Parse()
	if *port < 1 || *port > 65535 {
		log.Fatal("port must be in the range 1–65535")
	}
	target, err := url.Parse(*upstream)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") ||
		target.User != nil || target.ForceQuery || target.RawQuery != "" || target.Fragment != "" {
		log.Fatal("upstream must be an HTTP(S) base URL without credentials, query or fragment")
	}
	cfg := truncationfix.Config{Enabled: true}.Resolve()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(*port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Addr: addr, Handler: newGateway(target, cfg),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second,
	}
	log.Printf("Helix Gateway | Truncation Detection & Structural Repair | http://%s/v1 | mode=%s", addr, cfg.WrapperMode)
	log.Fatal(server.Serve(idleWriteListener{Listener: listener, timeout: writeIdleTimeout}))
}

func newGateway(target *url.URL, cfg truncationfix.Config) http.Handler {
	stats := &gatewayStats{}
	cache := &toolRetryCache{entries: make(map[retryKey]time.Time)}
	base := strings.TrimRight(target.EscapedPath(), "/")
	if base == "" {
		base = "/v1"
	}
	transport := http.DefaultTransport
	if standard, ok := transport.(*http.Transport); ok {
		clone := standard.Clone()
		// The gateway only decodes a response when it processes that response.
		clone.DisableCompression = true
		transport = clone
	}
	proxy := &httputil.ReverseProxy{
		Transport: toolFallbackTransport{upstream: transport, cache: cache},
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme = target.Scheme
			p.Out.URL.Host = target.Host
			p.Out.Host = target.Host
			p.Out.URL.Path, p.Out.URL.RawPath = upstreamPath(base, p.In.URL)
			if requestFix(p.In) != nil {
				p.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if err := rewriteResponse(resp); err != nil {
				return err
			}
			if cfg.Enabled && cfg.WrapperMode != "repair-only" {
				setLoopbackCORS(resp.Header, resp.Request.Header.Get("Origin"))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			if cfg.Enabled {
				setLoopbackCORS(w.Header(), r.Header.Get("Origin"))
			}
			var failure responseFailure
			if errors.As(err, &failure) {
				writeError(w, http.StatusBadGateway, failure.message)
				return
			}
			if state := requestState(r); state != nil {
				state.fix.Note("upstream_transport")
			}
			writeError(w, http.StatusBadGateway, "The upstream API response could not be read.")
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if cfg.Enabled {
			setLoopbackCORS(w.Header(), r.Header.Get("Origin"))
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "enabled": cfg.Enabled, "mode": cfg.WrapperMode})
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		if cfg.Enabled {
			setLoopbackCORS(w.Header(), r.Header.Get("Origin"))
		}
		stats.serve(w, r)
	})
	mux.Handle("GET /v1/models", proxy)
	mux.Handle("POST /v1/completions", proxy)
	mux.Handle("POST /v1/embeddings", proxy)
	for path, method := range map[string]string{
		"/v1/models": "GET", "/v1/chat/completions": "POST", "/v1/completions": "POST",
		"/v1/embeddings": "POST",
		"/healthz":       "GET", "/stats": "GET",
	} {
		if !cfg.Enabled && strings.HasPrefix(path, "/v1/") {
			mux.Handle("OPTIONS "+path, proxy)
			continue
		}
		mux.HandleFunc("OPTIONS "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", method+", OPTIONS")
			if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
				writeError(w, http.StatusForbidden, "Browser preflight is available only to loopback origins.")
				return
			}
			if cfg.Enabled {
				setLoopbackCORS(w.Header(), r.Header.Get("Origin"))
			}
			w.Header().Set("Access-Control-Allow-Methods", method+", OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key")
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var state *chatState
		writer, tracksDelivery := w.(*deliveryWriter)
		reason := "disabled"
		defer func() {
			var fix *truncationfix.Request
			if state != nil {
				fix = state.fix
				if state.terminal != nil && tracksDelivery && !writer.failed.Load() && r.Context().Err() == nil {
					fix.Record(*state.terminal)
				}
			}
			stats.record(fix, reason)
		}()
		if cfg.Enabled {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err != nil {
				setLoopbackCORS(w.Header(), r.Header.Get("Origin"))
				reason = "request_read"
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					reason = "request_limit"
					writeError(w, http.StatusRequestEntityTooLarge, "Request body exceeds the gateway limit.")
				} else {
					writeError(w, http.StatusBadRequest, "Request body could not be read.")
				}
				return
			}
			original := raw
			var fix *truncationfix.Request
			raw, fix, reason = cfg.Prepare(raw)
			var key retryKey
			if fix != nil && fix.Synthetic() {
				key = makeRetryKey(r, original)
				if cache.contains(key, time.Now()) {
					raw, fix, reason = original, nil, "upstream_tool_retry_suppressed"
				}
			}
			if fix != nil {
				state = &chatState{fix: fix, retryKey: key}
				if fix.Synthetic() {
					state.original = original
					state.acceptEncoding = r.Header.Values("Accept-Encoding")
				}
				r = r.WithContext(context.WithValue(r.Context(), requestKey{}, state))
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r.ContentLength = int64(len(raw))
		}
		proxy.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(&deliveryWriter{ResponseWriter: w}, r)
	})
}

// Match /v1 by its decoded first segment, but append the untouched escaped suffix.
func upstreamPath(base string, incoming *url.URL) (string, string) {
	first, suffix, hasSuffix := strings.Cut(strings.TrimPrefix(incoming.EscapedPath(), "/"), "/")
	segment, _ := url.PathUnescape(first)
	escaped := base
	if segment == "v1" && hasSuffix {
		escaped += "/" + suffix
	}
	decoded, _ := url.PathUnescape(escaped)
	return decoded, escaped
}

func setLoopbackCORS(header http.Header, origin string) {
	if !isLoopbackOrigin(origin) {
		return
	}
	header.Set("Access-Control-Allow-Origin", origin)
	for _, line := range header.Values("Vary") {
		for _, token := range strings.Split(line, ",") {
			name := strings.TrimSpace(token)
			if strings.EqualFold(name, "Origin") || name == "*" {
				return
			}
		}
	}
	header.Add("Vary", "Origin")
}

func isLoopbackOrigin(raw string) bool {
	origin, err := url.Parse(raw)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.User != nil ||
		origin.Path != "" || origin.ForceQuery || origin.RawQuery != "" || origin.Fragment != "" || origin.Host == "" {
		return false
	}
	host := origin.Hostname()
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func requestState(r *http.Request) *chatState {
	state, _ := r.Context().Value(requestKey{}).(*chatState)
	return state
}

func requestFix(r *http.Request) *truncationfix.Request {
	if state := requestState(r); state != nil && !state.retried {
		return state.fix
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message}})
}
