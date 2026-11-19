package logging

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// capture routes the default logger into a buffer for the test.
func capture(t *testing.T, format string, debug bool) *syncBuffer {
	t.Helper()
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	buf := &syncBuffer{}
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	if _, err := Setup(buf, format, level); err != nil {
		t.Fatal(err)
	}
	return buf
}

func TestSetupRejectsUnknownFormat(t *testing.T) {
	if _, err := Setup(io.Discard, "yaml", slog.LevelInfo); err == nil {
		t.Fatal("unknown format accepted")
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"", 3, ""},
		{"abc", 3, "abc"},
		{"abc", 10, "abc"},
		{"abcd", 3, "abc…(truncated)"},
		{"abc", 0, "…(truncated)"},
		{"世界世界", 12, "世界世界"},
		{"世界世界", 7, "世界…(truncated)"}, // 7 bytes would cut the third character
		{"世界世界", 5, "世…(truncated)"},
		{"🎉🎉🎉", 6, "🎉…(truncated)"},
		{"é", 1, "…(truncated)"},
	} {
		got := Truncate(tc.in, tc.n)
		if got != tc.want || !utf8.ValidString(got) {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestCutAtRune(t *testing.T) {
	s := "a世🎉z" // 1 + 3 + 4 + 1 bytes
	for n, want := range map[int]string{
		-1: "", 0: "", 1: "a", 2: "a", 3: "a", 4: "a世", 5: "a世", 7: "a世", 8: "a世🎉", 9: "a世🎉z", 10: "a世🎉z", 1000: "a世🎉z",
	} {
		if got := CutAtRune(s, n); got != want {
			t.Errorf("CutAtRune(%q, %d) = %q, want %q", s, n, got, want)
		}
	}
	// Invalid UTF-8 must not loop or panic.
	for n := 0; n < 5; n++ {
		CutAtRune("\xff\xfe\xfd\xfc", n)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"": slog.LevelInfo, "info": slog.LevelInfo, "INFO": slog.LevelInfo, " debug ": slog.LevelDebug,
		"warn": slog.LevelWarn, "Warning": slog.LevelWarn, "error": slog.LevelError,
	} {
		if got, err := ParseLevel(in); err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("trace"); err == nil || !strings.Contains(err.Error(), LevelNames) {
		t.Errorf("unknown level: %v", err)
	}
}

func TestLevelFiltersOutput(t *testing.T) {
	buf := &syncBuffer{}
	old := slog.Default()
	defer slog.SetDefault(old)
	logger, err := Setup(buf, FormatText, slog.LevelWarn)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hidden")
	logger.Warn("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestIsSecretName(t *testing.T) {
	for name, want := range map[string]bool{
		"Authorization": true, "x-portkey-api-key": true, "api-key": true, "X-API-KEY": true,
		"x-portkey-virtual-key": true, "Cookie": true, "access_token": true, "client_secret": true,
		"x-portkey-provider": false, "Content-Type": false, "x-portkey-config": false,
		"api-version": false, "User-Agent": false, "monkey": false,
	} {
		if got := IsSecretName(name); got != want {
			t.Errorf("IsSecretName(%q) = %v, want %v", name, got, want)
		}
	}
	if !IsSecretName("X-Gateway-Pass", "x-gateway-pass") {
		t.Error("extra secret names are ignored")
	}
}

func TestHeadersRedaction(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-secret")
	h.Set("api-key", "abc123")
	h.Set("X-Gateway-Pass", "hunter2")
	h.Set("Content-Type", "application/json")
	got := Headers(h, "X-Gateway-Pass")
	if got["Authorization"] != "Bearer [REDACTED]" || got["Api-Key"] != redacted || got["X-Gateway-Pass"] != redacted || got["Content-Type"] != "application/json" {
		t.Errorf("unexpected: %v", got)
	}
}

func TestURLValuedHeadersAreRedacted(t *testing.T) {
	h := http.Header{}
	h.Set("Location", "https://other.example/x?token=abc&page=2")
	h.Set("Referer", "http://origin.example/y?api-key=zzz")
	got := Headers(h)
	if got["Location"] != "https://other.example/x?page=2&token=[REDACTED]" || got["Referer"] != "http://origin.example/y?api-key=[REDACTED]" {
		t.Errorf("unexpected: %v", got)
	}
}

func TestURLRedaction(t *testing.T) {
	for in, want := range map[string]string{
		"https://gw.example/v1/chat/completions":                     "https://gw.example/v1/chat/completions",
		"https://gw.example/chat?api-version=2024-02&api-key=s3cret": "https://gw.example/chat?api-key=[REDACTED]&api-version=2024-02",
		"https://user:pw@gw.example/chat":                            "https://user:%5BREDACTED%5D@gw.example/chat",
		"/v1/completions?token=abc":                                  "/v1/completions?token=[REDACTED]",
	} {
		if got := URLString(in); got != want {
			t.Errorf("URLString(%q) = %q, want %q", in, got, want)
		}
	}
	if got := URLString("http://[bad"); strings.Contains(got, "bad") {
		t.Errorf("unparsable URL echoed: %q", got)
	}
	u, _ := url.Parse("https://x.example/a?key=1")
	URL(u)
	if u.RawQuery != "key=1" {
		t.Error("URL() modified its argument")
	}
}

func TestMiddlewareLogsRequestAndResponse(t *testing.T) {
	buf := capture(t, FormatText, false)
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		From(r.Context()).Info("inside handler")
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, "short and stout")
		w.(http.Flusher).Flush() // streaming must keep working through the wrapper
	}))
	req := httptest.NewRequest("POST", "/v1/completions?api-key=s3cret&x=1", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sk-secret")
	req.Header.Set("User-Agent", "zed/1.0")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	out := buf.String()
	for _, want := range []string{
		`msg="request received"`, "method=POST", "path=/v1/completions", "user_agent=zed/1.0", "content_length=2",
		`query="?api-key=[REDACTED]&x=1"`, `msg="response sent"`, "status=418", "bytes=15", "duration=", `msg="inside handler"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "sk-secret") {
		t.Errorf("secret leaked:\n%s", out)
	}
	// Info level must not dump headers; that is for debug.
	if strings.Contains(out, "headers.") {
		t.Errorf("headers logged at info level:\n%s", out)
	}
	ids := regexp.MustCompile(`request_id=(\S+)`).FindAllStringSubmatch(out, -1)
	if len(ids) != 3 || ids[0][1] != ids[1][1] || ids[1][1] != ids[2][1] {
		t.Errorf("request_id missing or inconsistent: %v", ids)
	}
	if w.Header().Get(RequestIDHeader) != ids[0][1] {
		t.Errorf("response header %q does not match logged id %q", w.Header().Get(RequestIDHeader), ids[0][1])
	}
}

func TestMiddlewareRequestID(t *testing.T) {
	buf := capture(t, FormatJSON, false)
	h := Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, tc := range []struct{ sent, want string }{
		{"abc-123_x.y", "abc-123_x.y"},
		{"has space", ""},
		{"evil\nlevel=ERROR", ""},
		{strings.Repeat("a", 65), ""},
	} {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set(RequestIDHeader, tc.sent)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		got := w.Header().Get(RequestIDHeader)
		if tc.want != "" && got != tc.want {
			t.Errorf("valid id %q replaced by %q", tc.sent, got)
		}
		if tc.want == "" && (got == tc.sent || len(got) != 12) {
			t.Errorf("invalid id %q accepted as %q", tc.sent, got)
		}
	}
	if strings.Contains(buf.String(), "evil") {
		t.Error("untrusted request id reached the log")
	}
	if !strings.Contains(buf.String(), `"msg":"request received"`) {
		t.Errorf("json log expected:\n%s", buf.String())
	}
}

func TestMiddlewareHealthIsDebugOnly(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	buf := capture(t, FormatText, false)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health", nil))
	if buf.String() != "" {
		t.Errorf("health check logged at info level:\n%s", buf.String())
	}
	buf = capture(t, FormatText, true)
	req := httptest.NewRequest("GET", "/health", nil)
	req.Header.Set("Authorization", "Bearer sk-secret")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(buf.String(), "request received") {
		t.Errorf("health check missing at debug level:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `headers.Authorization="Bearer [REDACTED]"`) || strings.Contains(buf.String(), "sk-secret") {
		t.Errorf("redacted headers expected at debug level:\n%s", buf.String())
	}
}

func TestTransportLogsRequestAndResponse(t *testing.T) {
	buf := capture(t, FormatText, false)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "upstream-77")
		w.Header().Set("Set-Cookie", "session=topsecret")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "{}")
	}))
	defer backend.Close()
	client := &http.Client{Transport: &Transport{Base: http.DefaultTransport, SecretHeaders: []string{"X-Gateway-Pass"}}}

	ctx := NewContext(context.Background(), slog.Default().With("request_id", "req-1"))
	req, _ := http.NewRequestWithContext(ctx, "POST", backend.URL+"/v1/chat/completions?api-key=s3cret", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sk-secret")
	req.Header.Set("X-Gateway-Pass", "hunter2")
	req.Header.Set("x-portkey-provider", "@my-slug")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	out := buf.String()
	for _, want := range []string{
		`msg="request sent to external service"`, "method=POST", "api-key=[REDACTED]", "headers.Authorization=\"Bearer [REDACTED]\"",
		"headers.X-Portkey-Provider=@my-slug", `msg="response received from external service"`, "status_code=200",
		"headers.X-Request-Id=upstream-77", "conn_reused=false", "time_to_first_byte=", "request_id=req-1", "level=INFO",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"s3cret", "sk-secret", "hunter2", "topsecret"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked:\n%s", secret, out)
		}
	}
}

func TestTransportLogsFailuresAndErrorStatuses(t *testing.T) {
	buf := capture(t, FormatText, false)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	client := &http.Client{Transport: &Transport{Base: http.DefaultTransport}}
	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "status_code=429") {
		t.Errorf("a 429 should be logged as a warning:\n%s", buf.String())
	}

	backend.Close()
	if _, err := client.Get(backend.URL); err == nil {
		t.Fatal("expected a connection error")
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, `msg="request to external service failed"`) || !strings.Contains(out, "connection refused") {
		t.Errorf("connection failure not logged:\n%s", out)
	}

	buf = capture(t, FormatText, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", backend.URL, nil)
	client.Do(req)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a cancelled request is a warning, not an error:\n%s", buf.String())
	}
}
