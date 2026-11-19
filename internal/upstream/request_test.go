package upstream

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestRequestShape(t *testing.T) {
	s := newServer(t, nil)
	c := mustClient(t, Config{URL: s.URL + "/v1/chat/completions?api-version=2024-02", AuthHeader: "api-key", APIKey: "k"})
	body := `{"model":"m","messages":[{"role":"user","content":"héllo 世界 🎉"}]}`
	if _, err := complete(t, c, body, ""); err != nil {
		t.Fatal(err)
	}
	r := s.only(t)
	if r.Method != http.MethodPost || r.Path != "/v1/chat/completions" || r.RawQuery != "api-version=2024-02" {
		t.Errorf("%s %s?%s", r.Method, r.Path, r.RawQuery)
	}
	if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
		t.Errorf("Content-Type %q, Accept %q", r.Header.Get("Content-Type"), r.Header.Get("Accept"))
	}
	if string(r.Body) != body || r.ContentLength != int64(len(body)) {
		t.Errorf("body not sent verbatim: %q (Content-Length %d)", r.Body, r.ContentLength)
	}
}

func TestLargeBodyIsSentIntact(t *testing.T) {
	s := newServer(t, nil)
	c := mustClient(t, Config{URL: s.URL})
	body := `{"p":"` + strings.Repeat("0123456789abcdef", 1<<16) + `"}` // ~1 MiB
	if _, err := complete(t, c, body, ""); err != nil {
		t.Fatal(err)
	}
	if got := s.only(t).Body; !bytes.Equal(got, []byte(body)) {
		t.Errorf("body corrupted: sent %d bytes, server got %d", len(body), len(got))
	}
}

func TestInvalidURLsFailWithoutSending(t *testing.T) {
	for _, url := range []string{"", "://bad", "http://[::1", "ftp://example.com/x", "example.com/no-scheme", "http://"} {
		t.Run(url, func(t *testing.T) {
			c := mustClient(t, Config{URL: url})
			if resp, err := complete(t, c, `{}`, ""); err == nil || resp != nil {
				t.Errorf("want an error, got resp=%v err=%v", resp, err)
			}
		})
	}
}

// authHeaders are every header a key could end up in, so each case can assert
// that the key went exactly where intended and nowhere else.
var authHeaders = []string{"Authorization", "Api-Key", "X-Portkey-Api-Key"}

func TestAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cfg        Config // URL is filled in
		editorAuth string // the Authorization header the editor sent to the proxy
		want       map[string]string
	}{
		// --- a configured key is always used, and beats the editor's
		{"configured key, default header", Config{APIKey: "K", AuthHeader: "Authorization"}, "", map[string]string{"Authorization": "Bearer K"}},
		{"header name is case-insensitive", Config{APIKey: "K", AuthHeader: "authorization"}, "", map[string]string{"Authorization": "Bearer K"}},
		{"header name in capitals", Config{APIKey: "K", AuthHeader: "AUTHORIZATION"}, "", map[string]string{"Authorization": "Bearer K"}},
		{"configured key, custom header gets the raw key", Config{APIKey: "K", AuthHeader: "api-key"}, "", map[string]string{"Api-Key": "K"}},
		{"portkey header", Config{APIKey: "K", AuthHeader: "x-portkey-api-key"}, "", map[string]string{"X-Portkey-Api-Key": "K"}},
		{"configured key beats the editor's", Config{APIKey: "K", AuthHeader: "api-key"}, "Bearer EDITOR", map[string]string{"Api-Key": "K"}},
		{"configured key beats the editor's, bearer header", Config{APIKey: "K", AuthHeader: "Authorization"}, "Bearer EDITOR", map[string]string{"Authorization": "Bearer K"}},
		{"empty header name falls back to Authorization", Config{APIKey: "K"}, "", map[string]string{"Authorization": "Bearer K"}},

		// --- no configured key: the editor's key is forwarded
		{"forward bearer key", Config{AuthHeader: "Authorization"}, "Bearer EK", map[string]string{"Authorization": "Bearer EK"}},
		{"scheme is case-insensitive", Config{AuthHeader: "Authorization"}, "bearer EK", map[string]string{"Authorization": "Bearer EK"}},
		{"scheme in capitals", Config{AuthHeader: "Authorization"}, "BEARER EK", map[string]string{"Authorization": "Bearer EK"}},
		{"extra whitespace is ignored", Config{AuthHeader: "Authorization"}, "  Bearer    EK  ", map[string]string{"Authorization": "Bearer EK"}},
		{"tab after the scheme", Config{AuthHeader: "Authorization"}, "Bearer\tEK", map[string]string{"Authorization": "Bearer EK"}},
		{"raw key without a scheme", Config{AuthHeader: "Authorization"}, "EK", map[string]string{"Authorization": "Bearer EK"}},
		{"forwarded to a custom header as the raw key", Config{AuthHeader: "api-key"}, "Bearer EK", map[string]string{"Api-Key": "EK"}},
		{"forwarded raw key to a custom header", Config{AuthHeader: "x-portkey-api-key"}, "EK", map[string]string{"X-Portkey-Api-Key": "EK"}},
		{"key containing spaces inside is kept whole", Config{AuthHeader: "api-key"}, "Bearer a b", map[string]string{"Api-Key": "a b"}},
		{"unicode-free but punctuated key", Config{AuthHeader: "api-key"}, "Bearer sk-ABC_123.x/y+z=", map[string]string{"Api-Key": "sk-ABC_123.x/y+z="}},

		// --- nothing to send
		{"no key anywhere sends no credential", Config{AuthHeader: "Authorization"}, "", map[string]string{}},
		{"whitespace-only editor header sends no credential", Config{AuthHeader: "api-key"}, "   ", map[string]string{}},
		{"bearer scheme without a token sends no credential", Config{AuthHeader: "Authorization"}, "Bearer", map[string]string{}},
		{"bearer scheme with only spaces sends no credential", Config{AuthHeader: "Authorization"}, "Bearer    ", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, nil)
			tc.cfg.URL = s.URL
			if _, err := complete(t, mustClient(t, tc.cfg), `{}`, tc.editorAuth); err != nil {
				t.Fatal(err)
			}
			got := s.only(t).Header
			for _, name := range authHeaders {
				if want, ok := tc.want[name]; ok {
					if got.Get(name) != want {
						t.Errorf("%s = %q, want %q", name, got.Get(name), want)
					}
				} else if v := got.Get(name); v != "" {
					t.Errorf("%s = %q, want it absent", name, v)
				}
			}
		})
	}
}

func TestCustomHeaders(t *testing.T) {
	s := newServer(t, nil)
	c := mustClient(t, Config{URL: s.URL, AuthHeader: "api-key", APIKey: "K", Headers: []string{
		"x-portkey-provider: @slug",
		"  X-Padded  :   value  ",
		"X-Colons: a:b:c",
		"X-Empty:",
		"X-Dup: first",
		"X-Dup: second",
		"Accept: text/event-stream",
		"Api-Key: from-custom-header",
	}})
	if _, err := complete(t, c, `{}`, ""); err != nil {
		t.Fatal(err)
	}
	h := s.only(t).Header
	for name, want := range map[string]string{
		"X-Portkey-Provider": "@slug",
		"X-Padded":           "value",
		"X-Colons":           "a:b:c", // only the first colon separates name and value
		"X-Dup":              "second",
		"Accept":             "text/event-stream",  // custom headers may override the defaults
		"Api-Key":            "from-custom-header", // ...and the key header, deliberately
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if v, ok := h["X-Empty"]; !ok || len(v) != 1 || v[0] != "" {
		t.Errorf("X-Empty = %q (present: %v), want one empty value", v, ok)
	}
}

func TestMalformedHeadersAreRejectedAtStartup(t *testing.T) {
	for _, h := range []string{"no-colon", "", ": value", "   : value", "bad name: v", "X-Bad\nInjected: v", "X-A: b\r\nX-Evil: 1"} {
		t.Run(h, func(t *testing.T) {
			if _, err := New(Config{URL: "http://x", Headers: []string{h}}); err == nil {
				t.Errorf("header %q was accepted", h)
			}
		})
	}
	if _, err := New(Config{URL: "http://x", Headers: []string{"X-Ok: 1", "Y: 2"}}); err != nil {
		t.Errorf("valid headers rejected: %v", err)
	}
}

func TestBearerToken(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "",
		"   ":              "",
		"Bearer":           "",
		"Bearer ":          "",
		"Bearer abc":       "abc",
		"bearer abc":       "abc",
		"  Bearer   abc  ": "abc",
		"Bearer\tabc":      "abc",
		"abc":              "abc",
		"Bearer a b":       "a b",
		"Bearerabc":        "Bearerabc", // not the scheme: no separator
		"Basic abc":        "Basic abc", // not ours to interpret
	} {
		if got := bearerToken(in); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}
