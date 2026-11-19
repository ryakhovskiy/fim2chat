package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recorder is a server that remembers the requests it received.
type recorder struct {
	*httptest.Server
	mu      sync.Mutex
	headers []http.Header
}

func newRecorder(t *testing.T, h http.HandlerFunc) *recorder {
	t.Helper()
	r := &recorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.headers = append(r.headers, req.Header.Clone())
		r.mu.Unlock()
		h(w, req)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) received() []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]http.Header(nil), r.headers...)
}

// localhostURL rewrites a 127.0.0.1 test-server URL to use the host name
// "localhost": a different host, which is what a cross-host redirect looks like.
func localhostURL(t *testing.T, serverURL string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(serverURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return "http://localhost:" + port
}

// A redirect must never be followed: Go would forward custom key headers
// (api-key, x-portkey-api-key...) to the new host, leaking the credential.
func TestRedirectIsNotFollowedAndKeyDoesNotLeak(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		authHeader string
	}{
		{"302 with custom key header", http.StatusFound, "x-portkey-api-key"},
		{"307 keeps the POST body", http.StatusTemporaryRedirect, "api-key"},
		{"308 with bearer auth", http.StatusPermanentRedirect, "Authorization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evil := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(`{"choices":[{"message":{"content":"from the redirect target"}}]}`))
			})
			origin := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, localhostURL(t, evil.URL)+"/steal?token=abc", tc.status)
			})
			client, err := New(Config{URL: origin.URL, APIKey: "super-secret-key", AuthHeader: tc.authHeader,
				Headers: []string{"x-portkey-virtual-key: virtual-secret"}})
			if err != nil {
				t.Fatal(err)
			}

			resp, err := client.Complete(context.Background(), []byte(`{}`), "")

			if got := evil.received(); len(got) != 0 {
				t.Fatalf("redirect target was contacted and received headers: %v", got)
			}
			var se *StatusError
			if resp != nil || !errors.As(err, &se) || se.Status != tc.status {
				t.Fatalf("want a StatusError %d, got resp=%v err=%v", tc.status, resp, err)
			}
			if se.Location != localhostURL(t, evil.URL)+"/steal?token=[REDACTED]" {
				t.Errorf("redirect target missing or not redacted in the error: %q", se.Location)
			}
			if !strings.Contains(se.Error(), "redirect") || strings.Contains(se.Error(), "abc") {
				t.Errorf("unhelpful or leaky message: %q", se.Error())
			}
			if n := len(origin.received()); n != 1 {
				t.Errorf("origin saw %d requests, want exactly 1", n)
			}
		})
	}
}

// A same-host redirect (http -> https upgrade, trailing-slash fix...) is not
// followed either: it is reported with its resolved target so the URL can be fixed.
func TestRelativeRedirectIsReportedNotFollowed(t *testing.T) {
	origin := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("redirect was followed to %s", r.URL.Path)
		}
		http.Redirect(w, r, "/v1/chat/completions/", http.StatusMovedPermanently)
	})
	client, err := New(Config{URL: origin.URL + "/v1/chat/completions", APIKey: "k", AuthHeader: "api-key"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Complete(context.Background(), []byte(`{}`), "")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusMovedPermanently || se.Location != origin.URL+"/v1/chat/completions/" {
		t.Fatalf("got %v (%+v)", err, se)
	}
	if n := len(origin.received()); n != 1 {
		t.Errorf("origin saw %d requests, want 1", n)
	}
}

// A non-redirect error keeps the plain message and no Location.
func TestErrorStatusWithoutRedirect(t *testing.T) {
	origin := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	client, _ := New(Config{URL: origin.URL})
	_, err := client.Complete(context.Background(), []byte(`{}`), "")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotModified || se.Location != "" || strings.Contains(se.Error(), "redirect") {
		t.Fatalf("got %v (%+v)", err, se)
	}
}
