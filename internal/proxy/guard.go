package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"ryakhovskiy/fim2chat/internal/logging"
)

// AnyHost, as an allowed host, turns the Host check off.
const AnyHost = "*"

// NormalizeHost reduces a Host header value or a configured host name to the
// bare lower-case name: no port, no IPv6 brackets, no trailing dot.
func NormalizeHost(hostport string) string {
	h := strings.TrimSpace(hostport)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// guard protects the listener from web pages and other unintended callers.
// The proxy holds an API key, so anything that can POST to it can spend the
// key's quota; being bound to localhost does not stop a browser on the same
// machine from doing exactly that.
type guard struct {
	allowedHosts map[string]bool
	anyHost      bool
	tokenHash    [sha256.Size]byte
	tokenSet     bool
}

func newGuard(cfg Config) *guard {
	g := &guard{allowedHosts: make(map[string]bool)}
	for _, h := range cfg.AllowedHosts {
		if h == AnyHost {
			g.anyHost = true
			continue
		}
		g.allowedHosts[NormalizeHost(h)] = true
	}
	if cfg.AuthToken != "" {
		g.tokenSet = true
		g.tokenHash = sha256.Sum256([]byte(cfg.AuthToken))
	}
	return g
}

// wrap applies, in order:
//
//  1. Host check, against DNS rebinding: the attacker's domain is made to
//     resolve to 127.0.0.1, so the browser considers the proxy same-origin, but
//     the Host header still carries the attacker's name. Loopback names and any
//     IP literal are accepted (rebinding needs a DNS name), plus AllowedHosts.
//  2. Browser check, against cross-site request forgery: a browser always sends
//     Origin on a cross-origin POST (and on same-origin ones), and Sec-Fetch-Site
//     on every request, and page scripts cannot remove or forge them. The editor
//     is not a browser and sends neither. This also rejects CORS preflights,
//     and no CORS headers are ever sent, so no page can read an answer either.
//  3. Shared token, if configured, as "Authorization: Bearer <token>".
//     /health is exempt so the Docker healthcheck keeps working.
func (g *guard) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := logging.From(r.Context())
		deny := func(status int, reason, message string, attrs ...any) {
			logger.Warn("request rejected", append([]any{"status", status, "reason", reason, "method", r.Method, "path", r.URL.Path}, attrs...)...)
			writeError(w, status, message)
		}

		if !g.hostAllowed(r.Host) {
			deny(http.StatusForbidden, "host not allowed", "host not allowed; see -allowed-hosts", "host", r.Host)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			deny(http.StatusForbidden, "browser request (Origin header present)", "requests from web pages are not allowed", "origin", origin)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "none" {
			deny(http.StatusForbidden, "browser request (Sec-Fetch-Site header present)", "requests from web pages are not allowed", "sec_fetch_site", site)
			return
		}
		if g.tokenSet && r.URL.Path != "/health" && !g.tokenOK(r) {
			// Never log the presented credential, only that it was wrong or missing.
			w.Header().Set("WWW-Authenticate", `Bearer realm="fim2chat"`)
			deny(http.StatusUnauthorized, "missing or invalid token", "missing or invalid token", "token_presented", r.Header.Get("Authorization") != "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g *guard) hostAllowed(hostport string) bool {
	if g.anyHost {
		return true
	}
	h := NormalizeHost(hostport)
	switch {
	case h == "":
		return false
	case h == "localhost", strings.HasSuffix(h, ".localhost"):
		return true
	case net.ParseIP(strings.SplitN(h, "%", 2)[0]) != nil: // IP literal, with or without an IPv6 zone
		return true
	}
	return g.allowedHosts[h]
}

func (g *guard) tokenOK(r *http.Request) bool {
	scheme, token, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	// Compare fixed-length hashes in constant time: no timing signal about the token.
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return subtle.ConstantTimeCompare(got[:], g.tokenHash[:]) == 1
}
