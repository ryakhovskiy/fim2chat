package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"ryakhovskiy/fim2chat/internal/logging"
)

// DefaultMaxResponseBytes caps how much of an upstream reply is read.
const DefaultMaxResponseBytes = 4 << 20

// ErrResponseTooLarge means the upstream reply was longer than the configured limit.
var ErrResponseTooLarge = errors.New("upstream response too large")

type Config struct {
	URL        string
	APIKey     string
	AuthHeader string
	Headers    []string
	Insecure   bool
	CACert     string
	// MaxResponseBytes limits the upstream reply; 0 means DefaultMaxResponseBytes.
	MaxResponseBytes int64
}

type Client struct {
	cfg  Config
	http *http.Client
}

// Response contains the first completion and its upstream metadata.
type Response struct {
	ID           string
	Model        string
	Created      int64
	Text         string
	FinishReason string
	Usage        json.RawMessage
}

type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage,omitempty"`
}

// bearerToken extracts the credential from the editor's Authorization header
// value: the token of a "Bearer <token>" value (any case, any whitespace), or
// the whole value when it has no scheme, since some editors send the raw key.
// A bare "Bearer" with no token yields nothing rather than the word "Bearer".
func bearerToken(authorization string) string {
	fields := strings.Fields(authorization)
	if len(fields) == 0 {
		return ""
	}
	if strings.EqualFold(fields[0], "bearer") {
		return strings.Join(fields[1:], " ")
	}
	return strings.Join(fields, " ")
}

func contentText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

type StatusError struct {
	Status int
	Body   []byte
	// Location is the redirect target (secrets redacted) when the upstream answered with a redirect.
	Location string
}

func (e *StatusError) Error() string {
	if e.Location != "" {
		return fmt.Sprintf("upstream answered %d and redirected to %s; redirects are not followed so the API key is never sent to another host - correct the upstream URL (-upstream)", e.Status, e.Location)
	}
	return fmt.Sprintf("upstream returned %d: %s", e.Status, truncate(string(e.Body), 500))
}

func (p *Client) Complete(ctx context.Context, body []byte, authorization string) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Use the configured key, otherwise forward the key Zed sent.
	key := p.cfg.APIKey
	if key == "" {
		key = bearerToken(authorization)
	}
	if key != "" {
		if header := p.cfg.AuthHeader; header == "" || strings.EqualFold(header, "Authorization") {
			req.Header.Set("Authorization", "Bearer "+key)
		} else {
			req.Header.Set(header, key)
		}
	}
	for _, h := range p.cfg.Headers {
		name, val, _ := strings.Cut(h, ":")
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(val))
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	logger := logging.From(ctx)
	limit := p.cfg.MaxResponseBytes
	if limit <= 0 {
		limit = DefaultMaxResponseBytes
	}
	readStart := time.Now()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		logger.Warn("reading response from external service failed", "url", logging.URL(req.URL), "bytes_read", len(data), "error", err.Error())
		return nil, err
	}
	logger.Debug("response body received from external service",
		"url", logging.URL(req.URL), "status_code", resp.StatusCode, "bytes", len(data),
		"read_duration", time.Since(readStart).Round(time.Microsecond), "body", logging.Truncate(string(data), 20000))
	tooLarge := int64(len(data)) > limit
	if tooLarge && resp.StatusCode/100 == 2 {
		return nil, fmt.Errorf("%w: more than %d bytes (raise -max-response-bytes / FIM_MAX_RESPONSE_BYTES)", ErrResponseTooLarge, limit)
	}
	if resp.StatusCode/100 != 2 {
		// An oversized error page is still an error status: keep the status and
		// the first part of the body rather than reporting "too large".
		if tooLarge {
			data = data[:limit]
		}
		se := &StatusError{Status: resp.StatusCode, Body: data}
		if loc, err := resp.Location(); err == nil {
			se.Location = logging.URL(loc)
		}
		return nil, se
	}
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return nil, fmt.Errorf("cannot parse upstream response: %v: %s", err, truncate(string(data), 500))
	}

	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("upstream response has no choices: %s", truncate(string(data), 500))
	}
	choice := cr.Choices[0]
	logger.Debug("upstream response parsed",
		"id", cr.ID, "model", cr.Model, "choices", len(cr.Choices),
		"finish_reason", choice.FinishReason, "usage", string(cr.Usage))
	return &Response{
		ID: cr.ID, Model: cr.Model, Created: cr.Created, Usage: cr.Usage,
		Text: contentText(choice.Message.Content), FinishReason: choice.FinishReason,
	}, nil
}

// validateHeader checks a configured "Name: value" header, so that a typo is
// reported at startup instead of on every request.
func validateHeader(h string) error {
	name, value, found := strings.Cut(h, ":")
	name = strings.TrimSpace(name)
	if !found || name == "" {
		return fmt.Errorf("invalid header %q: want 'Name: value'", h)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return fmt.Errorf("invalid header name %q", name)
		}
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("invalid value for header %s: control characters are not allowed", name)
	}
	return nil
}

// truncate shortens s to at most n bytes for an error message, without cutting
// a multi-byte character in half.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return logging.CutAtRune(s, n) + "…"
}

func New(cfg Config) (*Client, error) {
	for _, h := range cfg.Headers {
		if err := validateHeader(h); err != nil {
			return nil, err
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone() // honours HTTPS_PROXY / NO_PROXY
	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.Insecure}
	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading CA cert: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", cfg.CACert)
		}
		tlsCfg.RootCAs = pool
	}

	tr.TLSClientConfig = tlsCfg
	tr.MaxIdleConnsPerHost = 8
	// Every request to the upstream, and its response, is logged by the transport.
	logged := &logging.Transport{Base: tr, SecretHeaders: []string{cfg.AuthHeader}}
	return &Client{cfg: cfg, http: &http.Client{
		Transport: logged,
		// Never follow redirects. Go forwards custom headers such as api-key or
		// x-portkey-api-key to the redirect target (it strips only Authorization
		// and cookies, and only across hosts), which would hand the credential to
		// whatever host the upstream names. A redirect is reported as an error instead.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}
