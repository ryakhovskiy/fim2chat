package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- cancellation, deadlines and broken connections

func TestContextCancelledBeforeSending(t *testing.T) {
	s := newServer(t, nil)
	c := mustClient(t, Config{URL: s.URL})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := c.Complete(ctx, []byte(`{}`), "")
	if !errors.Is(err, context.Canceled) || resp != nil {
		t.Fatalf("resp=%v err=%v, want context.Canceled", resp, err)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("a cancelled request reached the server %d time(s)", n)
	}
}

func TestContextCancelledWhileWaiting(t *testing.T) {
	started := make(chan struct{})
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // answer never comes; the proxy hangs up
	})
	c := mustClient(t, Config{URL: s.URL})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()

	begin := time.Now()
	_, err := c.Complete(ctx, []byte(`{}`), "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(begin) > 3*time.Second {
		t.Errorf("cancellation took %v", time.Since(begin))
	}
}

func TestDeadlineWhileWaiting(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	c := mustClient(t, Config{URL: s.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Complete(ctx, []byte(`{}`), "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	var se *StatusError
	if errors.As(err, &se) {
		t.Error("a timeout is not an HTTP status")
	}
}

func TestDeadlineWhileReadingTheBody(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte(`{"choices":[`))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the rest never arrives
	})
	c := mustClient(t, Config{URL: s.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := c.Complete(ctx, []byte(`{}`), "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestConnectionProblems(t *testing.T) {
	t.Run("nothing is listening", func(t *testing.T) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		url := "http://" + l.Addr().String()
		l.Close()
		if resp, err := complete(t, mustClient(t, Config{URL: url}), `{}`, ""); err == nil || resp != nil {
			t.Fatalf("resp=%v err=%v", resp, err)
		}
	})
	t.Run("server closes the connection without answering", func(t *testing.T) {
		s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		})
		if _, err := complete(t, mustClient(t, Config{URL: s.URL}), `{}`, ""); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("server dies halfway through the body", func(t *testing.T) {
		s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "500")
			w.Write([]byte(`{"choices":[{"message":{"content":"abc`))
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		})
		resp, err := complete(t, mustClient(t, Config{URL: s.URL}), `{}`, "")
		if err == nil || resp != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("resp=%v err=%v, want io.ErrUnexpectedEOF", resp, err)
		}
	})
	t.Run("server sends garbage instead of HTTP", func(t *testing.T) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		defer l.Close()
		go func() {
			conn, err := l.Accept()
			if err == nil {
				io.WriteString(conn, "\x00\x01 this is not http\r\n\r\n")
				conn.Close()
			}
		}()
		if _, err := complete(t, mustClient(t, Config{URL: "http://" + l.Addr().String()}), `{}`, ""); err == nil {
			t.Fatal("want an error")
		}
	})
}

// --- connection handling

func TestConnectionsAreReused(t *testing.T) {
	var conns atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		switch r.URL.Query().Get("kind") {
		case "error":
			w.WriteHeader(500)
			io.WriteString(w, `{"error":"x"}`)
		case "garbage":
			io.WriteString(w, `not json`)
		default:
			reply(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}
	}))
	s.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	s.Start()
	defer s.Close()

	// Success, an error status, a malformed body, success: each must drain and
	// close its response so the connection goes back to the pool.
	for _, kind := range []string{"ok", "error", "garbage", "ok", "error", "ok"} {
		c := mustClientOnce(t, s.URL+"?kind="+kind)
		complete(t, c, `{}`, "")
	}
	// mustClientOnce shares one client, so reuse is measurable.
	if n := conns.Load(); n != 1 {
		t.Errorf("%d connections opened for 6 sequential requests, want 1", n)
	}
}

var (
	sharedMu      sync.Mutex
	sharedClients = map[string]*Client{}
)

// mustClientOnce returns one client per base URL (ignoring the query), so tests
// can issue several requests through the same connection pool.
func mustClientOnce(t *testing.T, url string) *Client {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	base, _, _ := strings.Cut(url, "?")
	key := base + "|" + t.Name()
	c, ok := sharedClients[key]
	if !ok {
		c = mustClient(t, Config{URL: base})
		sharedClients[key] = c
	}
	// Point the shared client at this request's full URL.
	clone := *c
	clone.cfg.URL = url
	return &clone
}

func TestConcurrentRequests(t *testing.T) {
	var inFlight, peak atomic.Int32
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		reply(w, fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, r.Header.Get("X-Id")))
	})
	c := mustClient(t, Config{URL: s.URL})
	const workers = 40
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each request carries its own id, so a mix-up between requests shows.
			cc := *c
			cc.cfg.Headers = []string{fmt.Sprintf("X-Id: id-%d", i)}
			resp, err := cc.Complete(context.Background(), []byte(`{}`), "")
			if err != nil {
				errs <- err
			} else if resp.Text != fmt.Sprintf("id-%d", i) {
				errs <- fmt.Errorf("request %d got the answer for %q", i, resp.Text)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if peak.Load() < 2 {
		t.Errorf("requests did not overlap (peak %d): the test proves nothing", peak.Load())
	}
}

// --- compression

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(s))
	zw.Close()
	return b.Bytes()
}

func TestGzipResponseIsDecoded(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("Accept-Encoding = %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gzipped(t, `{"choices":[{"message":{"content":"zipped 世界"}}]}`))
	})
	resp, err := complete(t, mustClient(t, Config{URL: s.URL}), `{}`, "")
	if err != nil || resp.Text != "zipped 世界" {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
}

// The limit applies to what is read after decompression, so a small response
// that expands enormously cannot exhaust memory.
func TestDecompressionBombIsStopped(t *testing.T) {
	bomb := gzipped(t, jsonOfSize(5<<20)) // ~5 KiB on the wire, 5 MiB decoded
	if len(bomb) > 64<<10 {
		t.Fatalf("test bomb is not small: %d bytes", len(bomb))
	}
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(bomb)
	})
	_, err := complete(t, mustClient(t, Config{URL: s.URL, MaxResponseBytes: 1 << 20}), `{}`, "")
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
}

func TestChunkedResponseWithoutContentLength(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		for _, part := range []string{`{"choices":[{"mess`, `age":{"content"`, `:"chunked"}}]}`} {
			io.WriteString(w, part)
			w.(http.Flusher).Flush()
		}
	})
	resp, err := complete(t, mustClient(t, Config{URL: s.URL}), `{}`, "")
	if err != nil || resp.Text != "chunked" {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
}

// --- TLS

// certPEM is the (self-signed) certificate of an httptest TLS server.
func certPEM(s *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTLSServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, `{"choices":[{"message":{"content":"secure"}}]}`)
	}))
	t.Cleanup(s.Close)
	return s
}

// newDistinctTLSServer starts a TLS server with a freshly generated certificate.
func newDistinctTLSServer(t *testing.T) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "distinct.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, `{"choices":[{"message":{"content":"distinct"}}]}`)
	}))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func TestTLS(t *testing.T) {
	srv := newTLSServer(t)
	other := newDistinctTLSServer(t) // httptest servers share one certificate; this one has its own

	if bytes.Equal(srv.Certificate().Raw, other.Certificate().Raw) {
		t.Fatal("test setup: both servers use the same certificate")
	}
	t.Run("an unknown certificate is rejected by default", func(t *testing.T) {
		_, err := complete(t, mustClient(t, Config{URL: srv.URL}), `{}`, "")
		var unknown interface{ Unwrap() error }
		if err == nil || !errors.As(err, &unknown) || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("err = %v, want a certificate verification error", err)
		}
	})
	t.Run("-insecure accepts it", func(t *testing.T) {
		resp, err := complete(t, mustClient(t, Config{URL: srv.URL, Insecure: true}), `{}`, "")
		if err != nil || resp.Text != "secure" {
			t.Fatalf("resp=%v err=%v", resp, err)
		}
	})
	t.Run("a CA file that contains the certificate is trusted", func(t *testing.T) {
		ca := writeFile(t, "ca.pem", certPEM(srv))
		resp, err := complete(t, mustClient(t, Config{URL: srv.URL, CACert: ca}), `{}`, "")
		if err != nil || resp.Text != "secure" {
			t.Fatalf("resp=%v err=%v", resp, err)
		}
	})
	t.Run("a CA file with several certificates", func(t *testing.T) {
		ca := writeFile(t, "ca.pem", append(append([]byte("# a comment\n"), certPEM(other)...), certPEM(srv)...))
		if _, err := complete(t, mustClient(t, Config{URL: srv.URL, CACert: ca}), `{}`, ""); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("trusting one certificate does not trust another", func(t *testing.T) {
		ca := writeFile(t, "ca.pem", certPEM(other))
		if _, err := complete(t, mustClient(t, Config{URL: srv.URL, CACert: ca}), `{}`, ""); err == nil {
			t.Fatal("a certificate that is not in the CA file was accepted")
		}
	})
	t.Run("a CA file does not switch verification off", func(t *testing.T) {
		ca := writeFile(t, "ca.pem", certPEM(other))
		c := mustClient(t, Config{URL: srv.URL, CACert: ca})
		if _, err := complete(t, c, `{}`, ""); err == nil {
			t.Fatal("verification is off")
		}
	})
	t.Run("a missing CA file is a startup error", func(t *testing.T) {
		_, err := New(Config{URL: srv.URL, CACert: filepath.Join(t.TempDir(), "nope.pem")})
		if err == nil || !strings.Contains(err.Error(), "CA cert") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a CA file without certificates is a startup error", func(t *testing.T) {
		for name, content := range map[string]string{"empty": "", "garbage": "not a certificate", "key only": "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"} {
			path := writeFile(t, "ca.pem", []byte(content))
			if _, err := New(Config{URL: srv.URL, CACert: path}); err == nil || !strings.Contains(err.Error(), "no certificates") {
				t.Errorf("%s: err = %v", name, err)
			}
		}
	})
	t.Run("an unreadable path is not mistaken for a certificate", func(t *testing.T) {
		if _, err := New(Config{URL: srv.URL, CACert: t.TempDir()}); err == nil { // a directory
			t.Fatal("a directory was accepted as a CA file")
		}
	})
}

func TestTLSMinimumVersion(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reply(w, `{"choices":[{"message":{"content":"x"}}]}`) }))
	srv.TLS = &tls.Config{MaxVersion: tls.VersionTLS11} // a server that only speaks obsolete TLS
	srv.StartTLS()
	defer srv.Close()
	_, err := complete(t, mustClient(t, Config{URL: srv.URL, Insecure: true}), `{}`, "")
	if err == nil {
		t.Error("TLS 1.1 was negotiated")
	}
}

func TestNewDoesNotShareTransports(t *testing.T) {
	// Two clients with different TLS settings must not influence each other
	// (the default transport is cloned, not mutated).
	srv := newTLSServer(t)
	strict := mustClient(t, Config{URL: srv.URL})
	lax := mustClient(t, Config{URL: srv.URL, Insecure: true})
	if _, err := complete(t, lax, `{}`, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := complete(t, strict, `{}`, ""); err == nil {
		t.Error("the strict client inherited the lax client's settings")
	}
	if http.DefaultTransport.(*http.Transport).TLSClientConfig != nil && http.DefaultTransport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Error("http.DefaultTransport was modified")
	}
}
