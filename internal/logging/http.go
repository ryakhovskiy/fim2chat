package logging

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

// RequestIDHeader carries the correlation ID. A sane value sent by the client
// is reused; otherwise one is generated. It is echoed on every response.
const RequestIDHeader = "X-Request-Id"

// Middleware logs every request the service receives ("request received") and
// the answer it sends back ("response sent"), and puts a logger carrying a
// request ID into the request context. Health checks are logged at debug level
// because Docker calls them every few seconds.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := requestID(r)
		logger := slog.Default().With("request_id", id)
		ctx := NewContext(r.Context(), logger)
		r = r.WithContext(ctx)
		w.Header().Set(RequestIDHeader, id)

		level := slog.LevelInfo
		if r.URL.Path == "/health" {
			level = slog.LevelDebug
		}
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"proto", r.Proto,
			"content_length", r.ContentLength,
			"user_agent", r.UserAgent(),
		}
		if r.URL.RawQuery != "" {
			q := *r.URL
			q.Path, q.RawPath = "", ""
			attrs = append(attrs, "query", URL(&q))
		}
		if logger.Enabled(ctx, slog.LevelDebug) {
			attrs = append(attrs, HeadersAttr(r.Header))
		}
		logger.Log(ctx, level, "request received", attrs...)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		logger.Log(ctx, level, "response sent",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration", time.Since(start).Round(time.Microsecond),
		)
	})
}

func requestID(r *http.Request) string {
	if id := r.Header.Get(RequestIDHeader); validRequestID(id) {
		return id
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// validRequestID accepts short printable tokens, so a client cannot inject
// newlines or oversized values into the log.
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// statusRecorder remembers what was written to the client.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush keeps streaming responses working through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
