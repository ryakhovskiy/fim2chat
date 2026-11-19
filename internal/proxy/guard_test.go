package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ryakhovskiy/fim2chat/internal/proxy"
	"ryakhovskiy/fim2chat/internal/upstream"
)

// guarded builds a proxy whose upstream counts the calls it receives, which is
// the quota an attacker would be spending.
func guarded(t *testing.T, cfg proxy.Config) (h http.Handler, upstreamCalls *atomic.Int32, lastKey *atomic.Value) {
	t.Helper()
	upstreamCalls, lastKey = new(atomic.Int32), new(atomic.Value)
	h = newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		lastKey.Store(r.Header.Get("api-key"))
		w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
	}, cfg, upstream.Config{AuthHeader: "api-key", APIKey: "upstream-key"})
	return h, upstreamCalls, lastKey
}

// request sends a completion request with the given Host and extra headers.
func request(h http.Handler, method, target, host string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(`{"prompt":"x"}`))
	r.Host = host
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const completions = "/v1/completions"

// The two attacks from the review: both used to reach the upstream and spend
// the victim's quota with HTTP 200.
func TestWebPagesCannotSpendTheQuota(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		host    string
		headers map[string]string
		want    int
	}{
		{"CSRF: cross-origin simple POST (text/plain)", "POST", "127.0.0.1:8787",
			map[string]string{"Origin": "https://evil.example", "Content-Type": "text/plain"}, 403},
		{"opaque origin (sandboxed iframe, file://)", "POST", "127.0.0.1:8787", map[string]string{"Origin": "null"}, 403},
		{"CORS preflight", "OPTIONS", "127.0.0.1:8787",
			map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"}, 403},
		{"DNS rebinding: attacker's name in Host", "POST", "evil.example:8787", nil, 403},
		{"DNS rebinding with the browser's Origin", "POST", "evil.example:8787", map[string]string{"Origin": "http://evil.example:8787"}, 403},
		{"look-alike of localhost", "POST", "localhost.evil.example:8787", nil, 403},
		{"Sec-Fetch-Site: cross-site without Origin", "POST", "127.0.0.1:8787", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"Sec-Fetch-Site: same-origin", "POST", "127.0.0.1:8787", map[string]string{"Sec-Fetch-Site": "same-origin"}, 403},
		{"empty Host", "POST", "", nil, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls, _ := guarded(t, proxy.Config{})
			w := request(h, tc.method, completions, tc.host, tc.headers)
			if w.Code != tc.want {
				t.Errorf("HTTP %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("the upstream was called %d time(s): quota spent", n)
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Error("CORS headers must never be sent")
			}
		})
	}
}

// The editor (and curl) are not browsers: no Origin, a loopback Host. They must
// keep working with no configuration.
func TestEditorStillWorks(t *testing.T) {
	for _, host := range []string{
		"127.0.0.1:8787", "127.0.0.1", "localhost:8787", "LOCALHOST:8787", "localhost.:8787", "[::1]:8787", "[::1]",
		"127.0.0.2:8787", "192.168.1.20:8787", "[fe80::1%25eth0]:8787", "app.localhost:8787",
	} {
		t.Run(host, func(t *testing.T) {
			h, calls, _ := guarded(t, proxy.Config{})
			// Sec-Fetch-Site: none is what a browser sends for a typed-in URL; harmless.
			w := request(h, "POST", completions, host, map[string]string{"Content-Type": "application/json"})
			if w.Code != 200 || calls.Load() != 1 {
				t.Errorf("Host %q: HTTP %d, upstream calls %d: %s", host, w.Code, calls.Load(), w.Body)
			}
		})
	}
}

func TestAllowedHosts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed []string
		host    string
		want    int
	}{
		{"unlisted name is refused", nil, "proxy.corp.example:8787", 403},
		{"listed name", []string{"proxy.corp.example"}, "proxy.corp.example:8787", 200},
		{"match ignores case and port", []string{"Proxy.Corp.Example:9999"}, "PROXY.corp.example:8787", 200},
		{"other names stay refused", []string{"proxy.corp.example"}, "evil.example:8787", 403},
		{"a subdomain is not the listed name", []string{"corp.example"}, "evil.corp.example:8787", 403},
		{"wildcard turns the check off", []string{proxy.AnyHost}, "anything.example", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := guarded(t, proxy.Config{AllowedHosts: tc.allowed})
			if w := request(h, "POST", completions, tc.host, nil); w.Code != tc.want {
				t.Errorf("HTTP %d, want %d", w.Code, tc.want)
			}
		})
	}
	// The wildcard relaxes only the Host check; web pages are still turned away.
	h, calls, _ := guarded(t, proxy.Config{AllowedHosts: []string{proxy.AnyHost}})
	if w := request(h, "POST", completions, "anything.example", map[string]string{"Origin": "https://evil.example"}); w.Code != 403 || calls.Load() != 0 {
		t.Errorf("Origin must still be refused: HTTP %d, calls %d", w.Code, calls.Load())
	}
}

func TestSharedToken(t *testing.T) {
	const token = "s3cr3t-token-value"
	for _, tc := range []struct {
		name string
		auth string
		want int
	}{
		{"correct token", "Bearer " + token, 200},
		{"scheme is case-insensitive", "bearer " + token, 200},
		{"missing", "", 401},
		{"wrong token", "Bearer nope", 401},
		{"token is a prefix of the real one", "Bearer " + token[:5], 401},
		{"real token with a suffix", "Bearer " + token + "x", 401},
		{"wrong scheme", "Basic " + token, 401},
		{"no scheme", token, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls, lastKey := guarded(t, proxy.Config{AuthToken: token})
			headers := map[string]string{}
			if tc.auth != "" {
				headers["Authorization"] = tc.auth
			}
			w := request(h, "POST", completions, "127.0.0.1:8787", headers)
			if w.Code != tc.want {
				t.Fatalf("HTTP %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			if tc.want == 401 {
				if calls.Load() != 0 || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer") {
					t.Errorf("a refused request must not reach the upstream (calls %d) and must say how to authenticate (%q)", calls.Load(), w.Header().Get("WWW-Authenticate"))
				}
				return
			}
			// The editor's token must never be forwarded: the configured key is used.
			if got := lastKey.Load(); got != "upstream-key" {
				t.Errorf("upstream received api-key %q, want the configured key", got)
			}
		})
	}
}

func TestTokenDoesNotBlockHealthAndDoesNotReplaceTheOtherChecks(t *testing.T) {
	h, calls, _ := guarded(t, proxy.Config{AuthToken: "s3cr3t-token-value"})
	if w := request(h, "GET", "/health", "127.0.0.1:8787", nil); w.Code != 200 {
		t.Errorf("/health needs no token (Docker healthcheck): HTTP %d", w.Code)
	}
	// A page that somehow knew the token is still a web page.
	w := request(h, "POST", completions, "127.0.0.1:8787", map[string]string{
		"Authorization": "Bearer s3cr3t-token-value", "Origin": "https://evil.example"})
	if w.Code != 403 || calls.Load() != 0 {
		t.Errorf("Origin must be refused even with the token: HTTP %d, calls %d", w.Code, calls.Load())
	}
	w = request(h, "POST", completions, "evil.example:8787", map[string]string{"Authorization": "Bearer s3cr3t-token-value"})
	if w.Code != 403 || calls.Load() != 0 {
		t.Errorf("Host must be refused even with the token: HTTP %d, calls %d", w.Code, calls.Load())
	}
}

// Rejections are logged with the reason, and credentials never are.
func TestRejectionsAreLoggedWithoutCredentials(t *testing.T) {
	logs := captureLogs(t, false)
	h, _, _ := guarded(t, proxy.Config{AuthToken: "s3cr3t-token-value"})
	request(h, "POST", completions, "evil.example:8787", nil)
	request(h, "POST", completions, "127.0.0.1:8787", map[string]string{"Origin": "https://evil.example"})
	request(h, "POST", completions, "127.0.0.1:8787", map[string]string{"Authorization": "Bearer guess-1234"})
	request(h, "POST", completions, "127.0.0.1:8787", map[string]string{"Authorization": "Bearer s3cr3t-token-value-but-longer"})

	out := logs.String()
	for _, want := range []string{
		`reason="host not allowed"`, "host=evil.example:8787",
		`reason="browser request (Origin header present)"`, "origin=https://evil.example",
		`reason="missing or invalid token"`, "token_presented=true", "status=403", "status=401", "level=WARN",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"s3cr3t-token-value", "guess-1234", "upstream-key"} {
		if strings.Contains(out, secret) {
			t.Errorf("credential %q leaked into the log:\n%s", secret, out)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"Example.COM": "example.com", "example.com:8787": "example.com", "example.com.": "example.com",
		"[::1]:8787": "::1", "[::1]": "::1", "127.0.0.1:1": "127.0.0.1", " localhost ": "localhost", "": "",
	} {
		if got := proxy.NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}
