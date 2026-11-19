package proxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"ryakhovskiy/fim2chat/internal/logging"
	"ryakhovskiy/fim2chat/internal/proxy"
	"ryakhovskiy/fim2chat/internal/upstream"
)

func TestCompletionRoundTrip(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("api-key") != "editor-key" {
			t.Error("expected POST with the forwarded editor key")
		}
		var req struct {
			Model     string
			Messages  []struct{ Role, Content string }
			MaxTokens int `json:"max_completion_tokens"`
			Stream    bool
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Model != "test-model" || req.MaxTokens != 32 || req.Stream {
			t.Errorf("unexpected upstream request: %+v", req)
		}
		if len(req.Messages) != 2 || req.Messages[0].Content != "insert code" || !strings.Contains(req.Messages[1].Content, "left<CURSOR>right") {
			t.Errorf("unexpected messages: %+v", req.Messages)
		}
		io.WriteString(w, `{"id":"abc","created":10,"choices":[{"message":{"content":[{"text":"left"},{"text":" + rightSTOPdiscard"}]}}],"usage":{"total_tokens":9}}`)
	}))
	defer backend.Close()
	client, err := upstream.New(upstream.Config{URL: backend.URL, AuthHeader: "api-key"})
	if err != nil {
		t.Fatal(err)
	}
	handler := proxy.NewHandler(proxy.Config{DefaultMaxTokens: 32, MaxTokensField: "max_completion_tokens", Timeout: time.Second, SystemPrompt: "insert code"}, client)
	for _, tc := range []struct {
		path   string
		stream bool
	}{{"/v1/completions", false}, {"/completions", true}} {
		t.Run(tc.path, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"test-model","prompt":["<fim_prefix>left<fim_suffix>right<fim_middle>","ignored"],"stop":"STOP","stream":%t}`, tc.stream)
			req := newReq("POST", tc.path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer editor-key")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body)
			}
			data := w.Body.String()
			if tc.stream {
				if w.Header().Get("Content-Type") != "text/event-stream" || !strings.HasPrefix(data, "data: ") || !strings.HasSuffix(data, "\n\ndata: [DONE]\n\n") {
					t.Fatalf("unexpected stream: %q", data)
				}
				data = strings.TrimSuffix(strings.TrimPrefix(data, "data: "), "\n\ndata: [DONE]\n\n")
			} else if w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("unexpected content type: %v", w.Header())
			}
			var resp struct {
				ID, Model, Object string
				Created           int64
				Choices           []struct {
					Text         string
					FinishReason string `json:"finish_reason"`
				}
				Usage map[string]int
			}
			if err := json.Unmarshal([]byte(data), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.ID != "abc" || resp.Model != "test-model" || resp.Object != "text_completion" || resp.Created != 10 || resp.Usage["total_tokens"] != 9 || len(resp.Choices) != 1 || resp.Choices[0].Text != " + " || resp.Choices[0].FinishReason != "stop" {
				t.Errorf("unexpected response: %s", data)
			}
		})
	}
}

func TestUpstreamErrorPassthrough(t *testing.T) {
	const body = `{"error":"rate limit"}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, body)
	}))
	defer backend.Close()
	client, err := upstream.New(upstream.Config{URL: backend.URL})
	if err != nil {
		t.Fatal(err)
	}
	h := proxy.NewHandler(proxy.Config{Timeout: time.Second, MaxTokensField: "max_tokens"}, client)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newReq("POST", "/completions", strings.NewReader(`{"prompt":"x"}`)))
	if w.Code != http.StatusTooManyRequests || w.Body.String() != body {
		t.Errorf("got HTTP %d: %s", w.Code, w.Body)
	}
}

// hang answers only when the proxy gives up and closes the connection. The body
// is drained first because the server notices a closed connection only then.
func hang(_ http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	<-r.Context().Done()
}

func newProxy(t *testing.T, backend http.HandlerFunc, cfg proxy.Config, ucfg upstream.Config) http.Handler {
	t.Helper()
	srv := httptest.NewServer(backend)
	t.Cleanup(srv.Close)
	ucfg.URL = srv.URL
	client, err := upstream.New(ucfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = time.Second
	}
	cfg.MaxTokensField = "max_tokens"
	return proxy.NewHandler(cfg, client)
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newReq("POST", "/v1/completions", strings.NewReader(body)))
	return w
}

func TestOversizedRequestIsLoggedAndRejected(t *testing.T) {
	logs := captureLogs(t, false)
	called := false
	h := newProxy(t, func(http.ResponseWriter, *http.Request) { called = true }, proxy.Config{MaxBodyBytes: 64}, upstream.Config{})

	w := post(h, `{"prompt":"`+strings.Repeat("x", 200)+`"}`)
	if w.Code != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("got HTTP %d (upstream called: %v): %s", w.Code, called, w.Body)
	}
	if !strings.Contains(logs.String(), "request body truncated") || !strings.Contains(logs.String(), "limit_bytes=64") {
		t.Errorf("truncation not logged: %q", logs.String())
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content type %q", w.Header().Get("Content-Type"))
	}
}

func TestUpstreamTimeoutIs504(t *testing.T) {
	h := newProxy(t, hang,
		proxy.Config{Timeout: 50 * time.Millisecond}, upstream.Config{})
	if w := post(h, `{"prompt":"x"}`); w.Code != http.StatusGatewayTimeout {
		t.Fatalf("got HTTP %d: %s", w.Code, w.Body)
	}
}

func TestClientCancelIsNot200(t *testing.T) {
	h := newProxy(t, hang, proxy.Config{Timeout: 5 * time.Second}, upstream.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newReq("POST", "/v1/completions", strings.NewReader(`{"prompt":"x"}`)).WithContext(ctx))
	if w.Code != 499 {
		t.Fatalf("got HTTP %d: %s", w.Code, w.Body)
	}
}

func TestNonJSONUpstreamErrorIsWrapped(t *testing.T) {
	h := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "<html>gateway down</html>")
	}, proxy.Config{}, upstream.Config{})
	w := post(h, `{"prompt":"x"}`)
	var resp struct{ Error struct{ Message string } }
	if w.Code != 503 || w.Header().Get("Content-Type") != "application/json" || json.Unmarshal(w.Body.Bytes(), &resp) != nil ||
		!strings.Contains(resp.Error.Message, "gateway down") {
		t.Fatalf("got HTTP %d %q: %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
}

func TestOversizedUpstreamResponseIsLoggedAnd502(t *testing.T) {
	logs := captureLogs(t, false)
	h := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"`+strings.Repeat("x", 500)+`"}}]}`)
	}, proxy.Config{}, upstream.Config{MaxResponseBytes: 100})
	w := post(h, `{"prompt":"x"}`)
	if w.Code != http.StatusBadGateway || !strings.Contains(logs.String(), "max-response-bytes") {
		t.Fatalf("got HTTP %d, log %q", w.Code, logs.String())
	}
}

type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLogs routes the default logger into a buffer for the duration of the test.
func captureLogs(t *testing.T, debug bool) *logBuffer {
	t.Helper()
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	buf := &logBuffer{}
	if _, err := logging.Setup(buf, logging.FormatText, level(debug)); err != nil {
		t.Fatal(err)
	}
	return buf
}

// One completion must leave a complete, correlated trail in the log, in order:
// request received -> request sent -> response received -> response sent, all
// under the same request_id, and without any secret.
func TestRequestLifecycleIsLogged(t *testing.T) {
	logs := captureLogs(t, false)
	h := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Trace", "trace-9")
		io.WriteString(w, `{"id":"abc","choices":[{"message":{"content":"42"},"finish_reason":"stop"}],"usage":{"total_tokens":9}}`)
	}, proxy.Config{Model: "gpt-x"}, upstream.Config{AuthHeader: "x-gateway-key", APIKey: "upstream-secret"})

	req := newReq("POST", "/v1/completions", strings.NewReader(`{"model":"zed-model","prompt":"<fim_prefix>x := <fim_suffix>\n<fim_middle>","max_tokens":16}`))
	req.Header.Set("Authorization", "Bearer editor-secret")
	req.Header.Set(logging.RequestIDHeader, "trace-me")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body)
	}

	out := logs.String()
	order := []string{
		`msg="request received"`,
		`msg="completion requested"`,
		`msg="request sent to external service"`,
		`msg="response received from external service"`,
		`msg="completion produced"`,
		`msg="response sent"`,
	}
	pos := -1
	for _, want := range order {
		i := strings.Index(out, want)
		if i < 0 || i < pos {
			t.Fatalf("%s missing or out of order in:\n%s", want, out)
		}
		pos = i
	}
	for _, want := range []string{
		"client_model=zed-model", "model=gpt-x", "model_source=config", "format=star_coder",
		"headers.X-Upstream-Trace=trace-9", "finish_reason=stop", "usage=", "status=200", "path=/v1/completions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"editor-secret", "upstream-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked into the log:\n%s", secret, out)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.Contains(line, "request_id=trace-me") {
			t.Errorf("line without the request id: %s", line)
		}
	}
	// Source code is not logged at info level.
	if strings.Contains(out, "x := ") || strings.Contains(out, "completion=") {
		t.Errorf("code leaked at info level:\n%s", out)
	}
}

func TestDebugLevelLogsBodies(t *testing.T) {
	logs := captureLogs(t, true)
	h := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"MODEL_OUTPUT"}}]}`)
	}, proxy.Config{}, upstream.Config{})
	post(h, `{"prompt":"CLIENT_PROMPT"}`)

	out := logs.String()
	for _, want := range []string{"CLIENT_PROMPT", `msg="request body read"`, `msg="upstream request built"`,
		`msg="response body received from external service"`, "MODEL_OUTPUT", `msg="model output cleaned"`} {
		if !strings.Contains(out, want) {
			t.Errorf("debug log lacks %q:\n%s", want, out)
		}
	}
}

func TestRejectedAndFailedRequestsAreLogged(t *testing.T) {
	logs := captureLogs(t, false)
	h := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"bad model"}`)
	}, proxy.Config{}, upstream.Config{})

	h.ServeHTTP(httptest.NewRecorder(), newReq("GET", "/v1/completions", nil))
	post(h, `not json`)
	post(h, `{"prompt":"x"}`)
	h.ServeHTTP(httptest.NewRecorder(), newReq("GET", "/nope", nil))

	out := logs.String()
	for _, want := range []string{
		`reason="method not allowed"`, `reason="invalid JSON"`, `msg="upstream returned an error status"`,
		"status_code=400", `msg="unhandled request"`, "path=/nope", "status=404", "status=405",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	// Every inbound request is logged exactly once on arrival.
	if n := len(regexp.MustCompile(`msg="request received"`).FindAllString(out, -1)); n != 4 {
		t.Errorf("%d 'request received' lines for 4 requests:\n%s", n, out)
	}
}

// The editor gets a clear 502 (a redirect is no completion), the redirect target
// is never contacted, and neither the key nor the target's secrets reach the log.
func TestUpstreamRedirectIsNotFollowed(t *testing.T) {
	logs := captureLogs(t, false)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("redirect target was contacted with headers %v", r.Header)
	}))
	defer target.Close()
	h := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere?token=redirect-secret", http.StatusTemporaryRedirect)
	}, proxy.Config{}, upstream.Config{AuthHeader: "x-portkey-api-key", APIKey: "upstream-secret"})

	w := post(h, `{"prompt":"x"}`)

	var resp struct{ Error struct{ Message string } }
	if w.Code != http.StatusBadGateway || w.Header().Get("Content-Type") != "application/json" || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		t.Fatalf("got HTTP %d %q: %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if !strings.Contains(resp.Error.Message, "redirected to") || !strings.Contains(resp.Error.Message, "-upstream") {
		t.Errorf("message does not explain what to fix: %q", resp.Error.Message)
	}
	out := logs.String() + w.Body.String()
	for _, secret := range []string{"upstream-secret", "redirect-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked:\n%s", secret, out)
		}
	}
	if !strings.Contains(logs.String(), "redirect_followed=false") || !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("redirect not logged as expected:\n%s", logs.String())
	}
}

func level(debug bool) slog.Level {
	if debug {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// newReq builds a request the way the editor sends it: addressed to the
// loopback listener, so the Host header is a loopback address. (httptest's
// default Host, example.com, is exactly what the guard rejects.)
func newReq(method, path string, body io.Reader) *http.Request {
	return httptest.NewRequest(method, "http://127.0.0.1:8787"+path, body)
}

// The 4-stop limit is for the upstream only: every stop sequence the editor
// sent must still be applied to the answer locally.
func TestSendStopLimitsUpstreamButNotLocalStops(t *testing.T) {
	var sent []string
	h := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Stop []string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		sent = req.Stop
		io.WriteString(w, `{"choices":[{"message":{"content":"keep S6 drop"},"finish_reason":"stop"}]}`)
	}, proxy.Config{SendStop: true, DefaultMaxTokens: 8, SystemPrompt: "x"}, upstream.Config{})

	w := post(h, `{"prompt":"abc","stop":["S1","S2","S3","S4","S5","S6"]}`)
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body)
	}
	if len(sent) != 4 || sent[3] != "S4" {
		t.Errorf("upstream got stop %v, want the first 4", sent)
	}
	var resp struct{ Choices []struct{ Text string } }
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Choices[0].Text; got != "keep " {
		t.Errorf("text = %q, want it cut at the 6th stop sequence (%q)", got, "keep ")
	}
}
