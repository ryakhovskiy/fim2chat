package logging

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// Transport wraps an http.RoundTripper and logs every request it sends and
// every response (or failure) that comes back. Because it sits at the transport
// level it also sees redirects. Bodies are not logged here.
type Transport struct {
	Base http.RoundTripper
	// SecretHeaders names headers to redact in addition to the ones
	// recognised by IsSecretName (e.g. a custom auth header).
	SecretHeaders []string
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	logger := From(ctx)
	target := URL(req.URL)
	start := time.Now()

	var mu sync.Mutex
	var reused bool
	var remote string
	var ttfb time.Duration
	trace := &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) {
			mu.Lock()
			defer mu.Unlock()
			reused, remote = i.Reused, i.Conn.RemoteAddr().String()
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			defer mu.Unlock()
			ttfb = time.Since(start)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))

	logger.Info("request sent to external service",
		"method", req.Method,
		"url", target,
		"host", req.URL.Host,
		"content_length", req.ContentLength,
		HeadersAttr(req.Header, t.SecretHeaders...),
	)

	resp, err := t.Base.RoundTrip(req)
	elapsed := time.Since(start).Round(time.Microsecond)
	mu.Lock()
	conn := []any{"conn_reused", reused, "remote_addr", remote, "time_to_first_byte", ttfb.Round(time.Microsecond)}
	mu.Unlock()

	if err != nil {
		level := slog.LevelError
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			level = slog.LevelWarn
		}
		logger.Log(ctx, level, "request to external service failed",
			append([]any{"method", req.Method, "url", target, "error", err.Error(), "duration", elapsed}, conn...)...)
		return nil, err
	}

	level := slog.LevelInfo
	if resp.StatusCode >= 300 { // errors, and redirects, which callers do not follow
		level = slog.LevelWarn
	}
	logger.Log(ctx, level, "response received from external service",
		append([]any{
			"method", req.Method,
			"url", target,
			"status_code", resp.StatusCode,
			"status", resp.Status,
			"proto", resp.Proto,
			"content_length", resp.ContentLength,
			HeadersAttr(resp.Header, t.SecretHeaders...),
			"duration", elapsed,
		}, conn...)...)
	return resp, nil
}
