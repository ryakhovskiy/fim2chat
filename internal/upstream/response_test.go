package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func ok(t *testing.T, body string) *Response {
	t.Helper()
	s := newServer(t, answer(200, body))
	resp, err := complete(t, mustClient(t, Config{URL: s.URL}), `{}`, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return resp
}

func TestParsesAFullResponse(t *testing.T) {
	resp := ok(t, `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"gpt-x-2025",
		"choices":[{"index":0,"message":{"role":"assistant","content":"a + b"},"finish_reason":"stop","logprobs":null}],
		"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13},"system_fingerprint":"fp"}`)
	if resp.ID != "chatcmpl-1" || resp.Model != "gpt-x-2025" || resp.Created != 1700000000 ||
		resp.Text != "a + b" || resp.FinishReason != "stop" {
		t.Errorf("%+v", resp)
	}
	var usage map[string]int
	if err := json.Unmarshal(resp.Usage, &usage); err != nil || usage["total_tokens"] != 13 {
		t.Errorf("usage not preserved: %s (%v)", resp.Usage, err)
	}
}

func TestParsesLeanResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       Response // Usage is compared separately
		usage      string
	}{
		{"only the content", `{"choices":[{"message":{"content":"x"}}]}`, Response{Text: "x"}, ""},
		{"finish reason length", `{"choices":[{"message":{"content":"x"},"finish_reason":"length"}]}`, Response{Text: "x", FinishReason: "length"}, ""},
		{"finish reason null", `{"choices":[{"message":{"content":"x"},"finish_reason":null}]}`, Response{Text: "x"}, ""},
		{"usage null is kept as null", `{"choices":[{"message":{"content":"x"}}],"usage":null}`, Response{Text: "x"}, "null"},
		{"unknown fields are ignored", `{"zzz":[1,2],"choices":[{"message":{"content":"x","zzz":{}},"zzz":1}],"zzz":"y"}`, Response{Text: "x"}, ""},
		{"empty content", `{"choices":[{"message":{"content":""}}]}`, Response{}, ""},
		{"null content (refusal or tool call)", `{"choices":[{"message":{"content":null}}]}`, Response{}, ""},
		{"missing content", `{"choices":[{"message":{}}]}`, Response{}, ""},
		{"missing message", `{"choices":[{"finish_reason":"stop"}]}`, Response{FinishReason: "stop"}, ""},
		{"numeric content is not text", `{"choices":[{"message":{"content":42}}]}`, Response{}, ""},
		{"object content is not text", `{"choices":[{"message":{"content":{"a":1}}}]}`, Response{}, ""},
		{"JSON escapes", `{"choices":[{"message":{"content":"a\n\t\"b\" é 🎉 \\"}}]}`, Response{Text: "a\n\t\"b\" é 🎉 \\"}, ""},
		{"raw unicode", `{"choices":[{"message":{"content":"世界 🎉"}}]}`, Response{Text: "世界 🎉"}, ""},
		{"only the first choice is used", `{"choices":[{"message":{"content":"first"}},{"message":{"content":"second"}}]}`, Response{Text: "first"}, ""},
		{"whitespace around the JSON", "  \n" + `{"choices":[{"message":{"content":"x"}}]}` + "\n\n", Response{Text: "x"}, ""},
		{"UTF-8 BOM is not JSON", "", Response{}, "error"}, // handled in TestMalformed
	} {
		if tc.usage == "error" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			got := ok(t, tc.body)
			if got.Text != tc.want.Text || got.FinishReason != tc.want.FinishReason || got.ID != "" {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if string(got.Usage) != tc.usage {
				t.Errorf("usage = %q, want %q", got.Usage, tc.usage)
			}
		})
	}
}

func TestContentText(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"string", `"hello"`, "hello"},
		{"empty string", `""`, ""},
		{"parts are concatenated in order", `[{"type":"text","text":"a"},{"type":"text","text":" + "},{"type":"text","text":"b"}]`, "a + b"},
		{"parts without a type", `[{"text":"a"},{"text":"b"}]`, "ab"},
		{"non-text parts add nothing", `[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"b"}]`, "ab"},
		{"only non-text parts", `[{"type":"image_url","image_url":{}}]`, ""},
		{"empty array", `[]`, ""},
		{"null", `null`, ""},
		{"number", `7`, ""},
		{"true", `true`, ""},
		{"object", `{"text":"a"}`, ""},
		{"array of strings is not parts", `["a","b"]`, ""},
		{"truncated JSON", `"abc`, ""},
		{"empty input", ``, ""},
		{"unicode parts", `[{"text":"世"},{"text":"界🎉"}]`, "世界🎉"},
		{"escapes inside a part", `[{"text":"a\nb"}]`, "a\nb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contentText(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("contentText(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
	if got := contentText(nil); got != "" {
		t.Errorf("contentText(nil) = %q", got)
	}
}

func TestMalformedSuccessResponses(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"empty body", ``, "cannot parse"},
		{"whitespace body", "  \n", "cannot parse"},
		{"not JSON", `OK`, "cannot parse"},
		{"HTML error page with status 200", `<html><body>Gateway</body></html>`, "cannot parse"},
		{"truncated JSON", `{"choices":[{"message":{"content":"abc`, "cannot parse"},
		{"top-level array", `[{"choices":[]}]`, "cannot parse"},
		{"top-level string", `"x"`, "cannot parse"},
		{"UTF-8 BOM", "\xef\xbb\xbf" + `{"choices":[{"message":{"content":"x"}}]}`, "cannot parse"},
		{"choices is not an array", `{"choices":{"message":{"content":"x"}}}`, "cannot parse"},
		{"no choices field", `{"id":"x"}`, "no choices"},
		{"empty choices", `{"choices":[]}`, "no choices"},
		{"null choices", `{"choices":null}`, "no choices"},
		{"error object with status 200", `{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`, "no choices"},
		{"empty object", `{}`, "no choices"},
		{"null", `null`, "no choices"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, answer(200, tc.body))
			resp, err := complete(t, mustClient(t, Config{URL: s.URL}), `{}`, "")
			if err == nil || resp != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resp=%v err=%v, want an error containing %q", resp, err, tc.want)
			}
			var se *StatusError
			if errors.As(err, &se) {
				t.Errorf("a malformed 200 must not be reported as an HTTP status error: %v", err)
			}
		})
	}
}

func TestErrorMessagesQuoteTheBodyBriefly(t *testing.T) {
	body := `{"error":"` + strings.Repeat("e", 5000) + `"`
	s := newServer(t, answer(200, body))
	_, err := complete(t, mustClient(t, Config{URL: s.URL}), `{}`, "")
	if err == nil || len(err.Error()) > 700 || !strings.Contains(err.Error(), "eeee") {
		t.Fatalf("message should quote a bounded part of the body, got %d bytes: %.120q", len(err.Error()), err)
	}
}

func TestErrorMessagesNeverCutACharacterInHalf(t *testing.T) {
	// Every shift of a multi-byte body against the cut point.
	for shift := 0; shift < 4; shift++ {
		body := strings.Repeat("x", shift) + strings.Repeat("世界🎉", 400)
		s := newServer(t, answer(200, body))
		_, err := complete(t, mustClient(t, Config{URL: s.URL}), `{}`, "")
		if err == nil || !utf8.ValidString(err.Error()) {
			t.Fatalf("shift %d: invalid UTF-8 in %q", shift, err)
		}
		s = newServer(t, answer(502, body))
		_, err = complete(t, mustClient(t, Config{URL: s.URL}), `{}`, "")
		if err == nil || !utf8.ValidString(err.Error()) {
			t.Fatalf("shift %d (status error): invalid UTF-8 in %q", shift, err)
		}
	}
}

func TestStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"bad request", 400, `{"error":{"message":"max_tokens is not supported","type":"invalid_request_error"}}`},
		{"unauthorized", 401, `{"error":"invalid api key"}`},
		{"forbidden", 403, `{"error":"no access to this model"}`},
		{"not found", 404, `{"error":"model not found"}`},
		{"rate limited", 429, `{"error":"slow down"}`},
		{"server error", 500, `{"error":"boom"}`},
		{"bad gateway with an HTML page", 502, `<html><h1>502 Bad Gateway</h1></html>`},
		{"unavailable with an empty body", 503, ``},
		{"unusual status", 418, `teapot`},
		{"redirect without a Location", 304, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, answer(tc.status, tc.body))
			resp, err := complete(t, mustClient(t, Config{URL: s.URL}), `{}`, "")
			var se *StatusError
			if resp != nil || !errors.As(err, &se) {
				t.Fatalf("want a StatusError, got resp=%v err=%v", resp, err)
			}
			if se.Status != tc.status || string(se.Body) != tc.body {
				t.Errorf("status %d body %q, want %d %q", se.Status, se.Body, tc.status, tc.body)
			}
			if !strings.Contains(se.Error(), http.StatusText(tc.status)) && !strings.Contains(se.Error(), "returned") {
				t.Errorf("message: %q", se.Error())
			}
		})
	}
}

func TestStatusErrorMessage(t *testing.T) {
	long := &StatusError{Status: 500, Body: []byte(strings.Repeat("x", 2000))}
	if msg := long.Error(); len(msg) > 600 || !strings.HasPrefix(msg, "upstream returned 500: xxx") {
		t.Errorf("long body: %d bytes %.80q", len(msg), msg)
	}
	multi := &StatusError{Status: 500, Body: []byte(strings.Repeat("世", 600))}
	if !utf8.ValidString(multi.Error()) {
		t.Errorf("invalid UTF-8: %q", multi.Error())
	}
	redirect := &StatusError{Status: 302, Location: "https://x.example/y?token=[REDACTED]"}
	if msg := redirect.Error(); !strings.Contains(msg, "302") || !strings.Contains(msg, "redirected to https://x.example/y?token=[REDACTED]") || !strings.Contains(msg, "-upstream") {
		t.Errorf("redirect: %q", msg)
	}
	wrapped := errors.Join(errors.New("ctx"), long)
	var se *StatusError
	if !errors.As(wrapped, &se) || se != long {
		t.Error("StatusError must be matchable with errors.As")
	}
}

func TestResponseSizeLimit(t *testing.T) {
	const limit = 200
	for _, tc := range []struct {
		name string
		size int
		fail bool
	}{
		{"well under", limit - 100, false},
		{"one under", limit - 1, false},
		{"exactly at the limit", limit, false},
		{"one over", limit + 1, true},
		{"far over", limit * 50, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, answer(200, jsonOfSize(tc.size)))
			resp, err := complete(t, mustClient(t, Config{URL: s.URL, MaxResponseBytes: limit}), `{}`, "")
			if tc.fail {
				if !errors.Is(err, ErrResponseTooLarge) || resp != nil {
					t.Fatalf("want ErrResponseTooLarge, got resp=%v err=%v", resp, err)
				}
				for _, hint := range []string{"200", "-max-response-bytes", "FIM_MAX_RESPONSE_BYTES"} {
					if !strings.Contains(err.Error(), hint) {
						t.Errorf("message lacks %q: %v", hint, err)
					}
				}
			} else if err != nil || resp.Text == "" {
				t.Fatalf("want success, got resp=%v err=%v", resp, err)
			}
		})
	}
}

func TestDefaultResponseLimit(t *testing.T) {
	if DefaultMaxResponseBytes != 4<<20 {
		t.Fatalf("default limit changed to %d: update the README and .env.example", DefaultMaxResponseBytes)
	}
	for _, size := range []int{DefaultMaxResponseBytes, DefaultMaxResponseBytes + 1} {
		s := newServer(t, answer(200, jsonOfSize(size)))
		_, err := complete(t, mustClient(t, Config{URL: s.URL}), `{}`, "") // zero means the default
		if tooLarge := errors.Is(err, ErrResponseTooLarge); tooLarge != (size > DefaultMaxResponseBytes) {
			t.Errorf("size %d: err = %v", size, err)
		}
	}
	// A negative limit also means the default rather than "nothing fits".
	s := newServer(t, nil)
	if _, err := complete(t, mustClient(t, Config{URL: s.URL, MaxResponseBytes: -5}), `{}`, ""); err != nil {
		t.Errorf("negative limit: %v", err)
	}
}

// An oversized error page must not hide the status: a 429 or 502 with a large
// body is still a 429 or 502.
func TestOversizedErrorBodyKeepsTheStatus(t *testing.T) {
	s := newServer(t, answer(429, strings.Repeat("rate limited. ", 100)))
	_, err := complete(t, mustClient(t, Config{URL: s.URL, MaxResponseBytes: 100}), `{}`, "")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 429 {
		t.Fatalf("want a 429 StatusError, got %v", err)
	}
	if len(se.Body) > 100 || !bytes.HasPrefix(se.Body, []byte("rate limited.")) {
		t.Errorf("body should be the first %d bytes, got %d bytes", 100, len(se.Body))
	}
	if errors.Is(err, ErrResponseTooLarge) {
		t.Error("an error status must not be reported as 'too large'")
	}
}

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"", 5, ""},
		{"abc", 5, "abc"},
		{"abcde", 5, "abcde"},
		{"abcdef", 5, "abcde…"},
		{"abc", 0, "…"},
		{"世界世界", 4, "世…"},    // 4 bytes: only the first 3-byte character fits
		{"世界世界", 6, "世界…"},   // exactly two characters
		{"世界世界", 12, "世界世界"}, // whole string
		{"🎉🎉", 5, "🎉…"},      // 4-byte characters
		{"é", 1, "…"},        // 2-byte character that does not fit
	} {
		got := truncate(tc.in, tc.n)
		if got != tc.want || !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
