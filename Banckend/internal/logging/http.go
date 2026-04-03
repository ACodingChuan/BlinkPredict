package logging

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const defaultBodyLogLimit = 64 * 1024

type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	body        bytes.Buffer
	bodyLimit   int
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if r.body.Len() < r.bodyLimit {
		remaining := r.bodyLimit - r.body.Len()
		if remaining > len(body) {
			remaining = len(body)
		}
		_, _ = r.body.Write(body[:remaining])
	}
	n, err := r.ResponseWriter.Write(body)
	r.bytes += n
	return n, err
}

func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (r *responseRecorder) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := r.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

func HTTPMiddleware(component string) func(http.Handler) http.Handler {
	logger := Component(component)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
			if requestID == "" {
				requestID = NewRequestID()
			}
			traceID := strings.TrimSpace(r.Header.Get("X-Trace-Id"))

			requestBody, requestTruncated := readRequestBody(r, defaultBodyLogLimit)
			ctx := WithRequestContext(r.Context(), requestID, traceID)
			r = r.WithContext(ctx)
			w.Header().Set("X-Request-Id", requestID)

			start := time.Now()
			recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK, bodyLimit: defaultBodyLogLimit}

			EventWithContext(ctx, logger.Info()).
				Str("kind", "http_request").
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Str("query", r.URL.RawQuery).
				Str("remote_addr", r.RemoteAddr).
				Any("headers", sanitizeHeaders(r.Header)).
				Str("body", prettyJSON(requestBody)).
				Bool("body_truncated", requestTruncated).
				Msg("http request received")

			next.ServeHTTP(recorder, r)

			routePattern := ""
			if routeCtx := chi.RouteContext(r.Context()); routeCtx != nil {
				routePattern = routeCtx.RoutePattern()
			}

			EventWithContext(ctx, logger.Info()).
				Str("kind", "http_response").
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Str("route_pattern", routePattern).
				Int("status", recorder.status).
				Int("bytes", recorder.bytes).
				Dur("latency", time.Since(start)).
				Any("headers", sanitizeHeaders(recorder.Header())).
				Str("body", prettyJSON(recorder.body.Bytes())).
				Bool("body_truncated", recorder.bytes > recorder.body.Len()).
				Msg("http response sent")
		})
	}
}

func readRequestBody(r *http.Request, limit int) ([]byte, bool) {
	if r == nil || r.Body == nil {
		return nil, false
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, int64(limit+1)))
	truncated := len(body) > limit
	if truncated {
		body = body[:limit]
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, truncated
}

func sanitizeHeaders(header http.Header) map[string][]string {
	if len(header) == 0 {
		return nil
	}
	clone := make(map[string][]string, len(header))
	for key, values := range header {
		lower := strings.ToLower(key)
		sanitized := append([]string(nil), values...)
		if lower == "authorization" || lower == "cookie" || lower == "set-cookie" {
			sanitized = []string{"[REDACTED]"}
		}
		clone[key] = sanitized
	}
	return clone
}

func prettyJSON(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return ""
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	formatted, err := json.Marshal(payload)
	if err != nil {
		return string(body)
	}
	return string(formatted)
}
