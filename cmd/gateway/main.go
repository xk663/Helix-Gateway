package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"helix-gateway-local/internal/truncationfix"
)

const (
	maxBody  = 32 << 20
	maxEvent = 8 << 20
)

type requestKey struct{}

func main() {
	upstream := flag.String("upstream", "http://127.0.0.1:8317/v1", "Upstream OpenAI-compatible API base URL")
	port := flag.Int("port", 8318, "Loopback HTTP port")
	flag.Parse()
	if *port < 1 || *port > 65535 {
		log.Fatal("port must be in the range 1–65535")
	}
	target, err := url.Parse(*upstream)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") ||
		target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		log.Fatal("upstream must be an HTTP(S) base URL without credentials, query or fragment")
	}
	cfg := truncationfix.Config{Enabled: true}.Resolve()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(*port))
	server := &http.Server{Addr: addr, Handler: newGateway(target, cfg), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("Helix Gateway listening at http://%s/v1", addr)
	log.Fatal(server.ListenAndServe())
}

func newGateway(target *url.URL, cfg truncationfix.Config) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme = target.Scheme
			p.Out.URL.Host = target.Host
			p.Out.Host = target.Host
			p.Out.URL.Path = strings.TrimRight(target.Path, "/") + strings.TrimPrefix(p.In.URL.Path, "/v1")
			p.Out.URL.RawPath = ""
			if fix, _ := p.In.Context().Value(requestKey{}).(*truncationfix.Request); fix != nil {
				p.Out.Header.Del("Accept-Encoding")
				p.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		ModifyResponse: rewriteResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			writeError(w, http.StatusBadGateway, "The upstream API response could not be read.")
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "enabled": cfg.Enabled, "mode": cfg.WrapperMode})
	})
	mux.Handle("GET /v1/models", proxy)
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if cfg.Enabled {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err != nil {
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					writeError(w, http.StatusRequestEntityTooLarge, "Request body exceeds the gateway limit.")
				} else {
					writeError(w, http.StatusBadRequest, "Request body could not be read.")
				}
				return
			}
			raw, fix := cfg.Inject(raw)
			r = r.WithContext(context.WithValue(r.Context(), requestKey{}, fix))
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r.ContentLength = int64(len(raw))
		}
		proxy.ServeHTTP(w, r)
	})
	return mux
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message}})
}

func rewriteResponse(resp *http.Response) error {
	fix, _ := resp.Request.Context().Value(requestKey{}).(*truncationfix.Request)
	if fix == nil || resp.StatusCode != http.StatusOK ||
		(resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity") {
		return nil
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch media {
	case "text/event-stream":
		resp.Body = &sseBody{ReadCloser: resp.Body, reader: bufio.NewReaderSize(resp.Body, fix.BufferSize()), stream: fix.Stream()}
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		resp.Header.Del("ETag")
	case "application/json":
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if len(raw) > maxBody {
			return fmt.Errorf("upstream response exceeds %d bytes", maxBody)
		}
		result := fix.Complete(raw)
		resp.Body = io.NopCloser(bytes.NewReader(result))
		resp.ContentLength = int64(len(result))
		resp.Header.Set("Content-Length", strconv.Itoa(len(result)))
		resp.Header.Del("ETag")
	}
	return nil
}

type sseBody struct {
	io.ReadCloser
	reader  *bufio.Reader
	stream  *truncationfix.Stream
	pending []byte
	err     error
}

func (body *sseBody) Read(p []byte) (int, error) {
	for len(body.pending) == 0 && body.err == nil {
		body.pending, body.err = body.next()
	}
	n := copy(p, body.pending)
	body.pending = body.pending[n:]
	if n > 0 {
		return n, nil
	}
	return n, body.err
}

func (body *sseBody) next() ([]byte, error) {
	var wire bytes.Buffer
	for {
		part, err := body.reader.ReadSlice('\n')
		if wire.Len()+len(part) > maxEvent {
			return nil, errors.New("upstream SSE event exceeds gateway limit")
		}
		wire.Write(part)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil || bytes.Equal(part, []byte("\n")) || bytes.Equal(part, []byte("\r\n")) {
			// Only an entire blank line terminates an event. A line spanning the
			// reader buffer can have a final segment containing only a newline.
			data := wire.Bytes()
			endsEvent := bytes.HasSuffix(data, []byte("\n\n")) ||
				bytes.HasSuffix(data, []byte("\n\r\n")) || len(data) <= 2
			if err != nil || endsEvent {
				return rewriteEvent(data, body.stream), err
			}
		}
	}
}

func rewriteEvent(wire []byte, stream *truncationfix.Stream) []byte {
	var data, other bytes.Buffer
	haveData := false
	for _, line := range bytes.SplitAfter(wire, []byte("\n")) {
		value := bytes.TrimRight(line, "\r\n")
		field, payload, _ := bytes.Cut(value, []byte(":"))
		if bytes.Equal(field, []byte("data")) {
			if haveData {
				data.WriteByte('\n')
			}
			haveData = true
			data.Write(bytes.TrimPrefix(payload, []byte(" ")))
		} else if len(value) > 0 {
			other.Write(line)
		}
	}
	raw := data.Bytes()
	if !haveData || bytes.Equal(raw, []byte("[DONE]")) {
		return wire
	}
	chunk := stream.Chunk(raw)
	if bytes.Equal(raw, chunk) {
		return wire
	}
	other.WriteString("data: ")
	other.Write(chunk)
	other.WriteString("\n\n")
	return other.Bytes()
}
