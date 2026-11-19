package logging

import (
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const redacted = "[REDACTED]"

// secretWords are the name parts that mark a header or query parameter as
// secret. Names are split on '-' and '_' and compared whole, so "x-portkey-api-key"
// is secret (it has "key") while "x-portkey-provider" is not.
var secretWords = map[string]bool{
	"authorization": true, "auth": true, "key": true, "apikey": true, "token": true,
	"secret": true, "password": true, "passwd": true, "cookie": true,
	"signature": true, "sig": true, "credential": true, "credentials": true,
}

// IsSecretName reports whether a header or parameter name looks like it carries
// a secret. extra lists names that are secret regardless (compared case-insensitively),
// e.g. the configured upstream auth header.
func IsSecretName(name string, extra ...string) bool {
	lower := strings.ToLower(name)
	for _, e := range extra {
		if strings.EqualFold(e, name) {
			return true
		}
	}
	for _, part := range strings.FieldsFunc(lower, func(r rune) bool { return r == '-' || r == '_' }) {
		if secretWords[part] {
			return true
		}
	}
	return false
}

// urlHeaders carry URLs, which may hold a secret in their query string.
var urlHeaders = map[string]bool{"location": true, "content-location": true, "referer": true}

// Headers returns the headers as a loggable map with secret values replaced.
// An Authorization value keeps its scheme ("Bearer [REDACTED]") because that
// is useful when debugging auth problems.
func Headers(h http.Header, extraSecret ...string) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		switch {
		case urlHeaders[strings.ToLower(name)] && !IsSecretName(name, extraSecret...):
			redactedURLs := make([]string, len(values))
			for i, v := range values {
				redactedURLs[i] = URLString(v)
			}
			out[name] = strings.Join(redactedURLs, ", ")
		case !IsSecretName(name, extraSecret...):
			out[name] = strings.Join(values, ", ")
		case strings.EqualFold(name, "Authorization"):
			scheme, _, found := strings.Cut(strings.TrimSpace(strings.Join(values, ", ")), " ")
			if found {
				out[name] = scheme + " " + redacted
			} else {
				out[name] = redacted
			}
		default:
			out[name] = redacted
		}
	}
	return out
}

// HeadersAttr is Headers as a log attribute named "headers": a nested object in
// JSON, and sorted headers.Name=value pairs in text.
func HeadersAttr(h http.Header, extraSecret ...string) slog.Attr {
	m := Headers(h, extraSecret...)
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	attrs := make([]any, 0, len(names))
	for _, name := range names {
		attrs = append(attrs, slog.String(name, m[name]))
	}
	return slog.Group("headers", attrs...)
}

// URL renders u for logging with the password and any secret query parameter
// values (api keys in the query string, signatures...) replaced.
func URL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	if c.User != nil {
		if _, hasPassword := c.User.Password(); hasPassword {
			c.User = url.UserPassword(c.User.Username(), redacted)
		}
	}
	if c.RawQuery != "" {
		q := c.Query()
		for name := range q {
			if IsSecretName(name) {
				q[name] = []string{redacted}
			}
		}
		// Encode escapes the brackets; keep the marker readable.
		c.RawQuery = strings.ReplaceAll(q.Encode(), url.QueryEscape(redacted), redacted)
	}
	return c.String()
}

// URLString is URL for a raw string; an unparsable value is not logged at all,
// since it might contain a secret.
func URLString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparsable url]"
	}
	return URL(u)
}
