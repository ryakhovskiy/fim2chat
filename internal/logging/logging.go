// Package logging provides the structured logging used across the service:
// logger setup, a request-scoped logger carried in the context, redaction of
// secrets, an HTTP middleware that logs every inbound request, and an
// http.RoundTripper that logs every outbound request and its response.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// Log formats accepted by Setup.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// ValidFormat reports whether format is one Setup understands.
func ValidFormat(format string) bool {
	return format == FormatText || format == FormatJSON
}

// LevelNames lists the accepted -log-level values, for messages.
const LevelNames = "debug, info, warn, error"

// ParseLevel converts a -log-level value (case-insensitive) to a slog level.
// An empty string means info.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want one of: %s)", name, LevelNames)
}

// Setup builds a logger writing to w, makes it the process default (so the
// standard library's log package is routed through it as well) and returns it.
// Lines below level are dropped. Debug-level lines include request and response
// bodies, which contain source code.
func Setup(w io.Writer, format string, level slog.Level) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch format {
	case FormatText, "":
		h = slog.NewTextHandler(w, opts)
	case FormatJSON:
		h = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q (want %s or %s)", format, FormatText, FormatJSON)
	}
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger, nil
}

type ctxKey struct{}

// NewContext returns ctx carrying logger.
func NewContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, logger)
}

// From returns the request-scoped logger stored in ctx (which already has the
// request ID attached), or the default logger if there is none.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// Truncate shortens s to at most n bytes for logging, without cutting a
// multi-byte character in half.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return CutAtRune(s, n) + "…(truncated)"
}

// CutAtRune returns the longest prefix of s that is at most n bytes long and
// ends on a character boundary.
func CutAtRune(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
