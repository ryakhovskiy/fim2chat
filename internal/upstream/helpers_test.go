package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// seen is one request as the server received it.
type seen struct {
	Method, Path, RawQuery string
	Header                 http.Header
	Body                   []byte
	ContentLength          int64
}

// server is a fake upstream that records every request.
type server struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []seen
}

// newServer starts a fake upstream. handler may be nil: it then answers with a
// minimal valid completion.
func newServer(t *testing.T, handler http.HandlerFunc) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body, r.ContentLength})
		s.mu.Unlock()
		if handler == nil {
			reply(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) requests() []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seen(nil), s.reqs...)
}

// only returns the single request the server saw, failing the test otherwise.
func (s *server) only(t *testing.T) seen {
	t.Helper()
	reqs := s.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	return reqs[0]
}

func reply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, body)
}

// answer is a handler that replies with a fixed status and body.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func mustClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// complete calls the client with a short deadline so a bug cannot hang the suite.
func complete(t *testing.T, c *Client, body, editorAuth string) (*Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.Complete(ctx, []byte(body), editorAuth)
}

// jsonOfSize returns a valid completion response of exactly n bytes.
func jsonOfSize(n int) string {
	const head, tail = `{"choices":[{"message":{"content":"`, `"}}]}`
	if n < len(head)+len(tail) {
		panic(fmt.Sprintf("size %d too small", n))
	}
	s := head + strings.Repeat("x", n-len(head)-len(tail)) + tail
	if !json.Valid([]byte(s)) || len(s) != n {
		panic("bad generator")
	}
	return s
}
