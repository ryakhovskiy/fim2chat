// Package config builds the server's settings from command-line flags,
// environment variables and an optional .env file.
//
// Precedence, highest first: command-line flag, process environment variable,
// .env file entry (see LoadDotEnv), built-in default.
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"ryakhovskiy/fim2chat/internal/fim"
	"ryakhovskiy/fim2chat/internal/logging"
	"ryakhovskiy/fim2chat/internal/proxy"
	"ryakhovskiy/fim2chat/internal/upstream"
)

// ErrMissingUpstream is returned by Parse when no upstream URL was configured,
// so callers can print usage instead of a bare error.
var ErrMissingUpstream = errors.New("set CORP_API_URL, FIM_UPSTREAM_URL, or -upstream")

// Config is everything the program needs to run.
type Config struct {
	Listen      string
	Healthcheck bool // probe a running server and exit instead of serving
	LogFormat   string
	LogLevel    slog.Level
	// Warnings are problems that do not stop startup (deprecated options in
	// use); the caller logs them once the logger is configured.
	Warnings []string
	Proxy    proxy.Config
	Upstream upstream.Config
}

// Parse registers the flags on flagSet, parses args, fills in unset flags from
// the environment, then validates and finishes the result. In healthcheck mode
// only Listen is resolved, so no upstream settings are required.
func Parse(flagSet *flag.FlagSet, args []string) (Config, error) {
	var cfg Config
	var extra extraOptions
	register(flagSet, &cfg, &extra)
	if err := flagSet.Parse(args); err != nil {
		return cfg, err
	}

	if extra.healthcheck {
		cfg.Healthcheck = true
		return cfg, resolveHealthcheckListen(flagSet, &cfg)
	}
	if err := applyEnv(flagSet); err != nil {
		return cfg, err
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, cfg.finish(extra, explicitFlags(flagSet))
}

func (c *Config) validate() error {
	if c.Upstream.URL == "" {
		return ErrMissingUpstream
	}
	if c.Proxy.AuthToken != "" && c.Upstream.APIKey == "" {
		return errors.New("-auth-token requires -api-key (CORP_API_KEY): without it the editor's token would be forwarded to the upstream as its API key")
	}
	if !logging.ValidFormat(c.LogFormat) {
		return fmt.Errorf("-log-format must be %q or %q", logging.FormatText, logging.FormatJSON)
	}
	if c.Proxy.DefaultMaxTokens <= 0 || c.Proxy.Timeout <= 0 || c.Proxy.MaxBodyBytes <= 0 || c.Upstream.MaxResponseBytes <= 0 {
		return errors.New("-max-tokens, -timeout, -max-body-bytes and -max-response-bytes must be positive")
	}
	return nil
}

// finish derives the settings that are not plain flag values: it applies
// implied options and reads the files and JSON the flags point to.
func (c *Config) finish(extra extraOptions, explicit map[string]bool) error {
	level, err := resolveLogLevel(extra)
	if err != nil {
		return err
	}
	c.LogLevel = level
	if explicit["debug"] {
		c.Warnings = append(c.Warnings, "-debug / FIM_DEBUG is deprecated: use -log-level debug / FIM_LOG_LEVEL=debug")
	}
	if explicit["v"] {
		c.Warnings = append(c.Warnings, "-v / FIM_VERBOSE is deprecated and has no effect: completion text is logged at -log-level debug / FIM_LOG_LEVEL=debug")
	}
	c.Upstream.Headers = extra.headers
	c.Proxy.AllowedHosts = parseHosts(extra.allowedHosts)
	if c.Proxy.AuthToken != "" && len(c.Proxy.AuthToken) < minTokenLength {
		c.Warnings = append(c.Warnings, fmt.Sprintf("-auth-token / FIM_AUTH_TOKEN is shorter than %d characters and easy to guess; use a long random value", minTokenLength))
	}
	if slices.Contains(c.Proxy.AllowedHosts, proxy.AnyHost) {
		c.Warnings = append(c.Warnings, "-allowed-hosts * disables the Host check that protects against DNS rebinding")
	}

	c.Proxy.SystemPrompt = fim.DefaultSystemPrompt
	if extra.systemPromptFile != "" {
		b, err := os.ReadFile(extra.systemPromptFile)
		if err != nil {
			return fmt.Errorf("reading system prompt: %w", err)
		}
		c.Proxy.SystemPrompt = string(b)
	}
	if extra.extraBodyJSON != "" {
		if err := json.Unmarshal([]byte(extra.extraBodyJSON), &c.Proxy.ExtraBody); err != nil {
			return fmt.Errorf("-extra-body must be a JSON object: %w", err)
		}
	}
	return nil
}

// minTokenLength is the length below which an -auth-token earns a warning.
const minTokenLength = 16

// parseHosts splits a comma-separated host list, normalizing each name.
func parseHosts(list string) []string {
	var hosts []string
	for _, h := range strings.Split(list, ",") {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		if h != proxy.AnyHost {
			h = proxy.NormalizeHost(h)
		}
		hosts = append(hosts, h)
	}
	return hosts
}

// resolveLogLevel picks the level: -log-level wins; otherwise the deprecated
// -debug means debug; otherwise info.
func resolveLogLevel(extra extraOptions) (slog.Level, error) {
	name := extra.logLevel
	if name == "" && extra.debug {
		name = "debug"
	}
	level, err := logging.ParseLevel(name)
	if err != nil {
		return 0, fmt.Errorf("-log-level: %w", err)
	}
	return level, nil
}

// LogValue implements slog.LogValuer so the effective configuration can be
// logged at startup. Secrets are never included: the API key is reduced to
// whether it is set, header values are dropped, and URLs are redacted.
func (c Config) LogValue() slog.Value {
	headerNames := make([]string, 0, len(c.Upstream.Headers))
	for _, h := range c.Upstream.Headers {
		name, _, _ := strings.Cut(h, ":")
		headerNames = append(headerNames, strings.TrimSpace(name))
	}
	return slog.GroupValue(
		slog.String("listen", c.Listen),
		slog.String("upstream_url", logging.URLString(c.Upstream.URL)),
		slog.Bool("api_key_configured", c.Upstream.APIKey != ""),
		slog.String("auth_header", c.Upstream.AuthHeader),
		slog.Any("extra_headers", headerNames),
		slog.String("model", c.Proxy.Model),
		slog.String("max_tokens_field", c.Proxy.MaxTokensField),
		slog.Int("default_max_tokens", c.Proxy.DefaultMaxTokens),
		slog.Float64("temperature", c.Proxy.Temperature),
		slog.Int("extra_body_fields", len(c.Proxy.ExtraBody)),
		slog.Bool("send_stop", c.Proxy.SendStop),
		slog.Duration("timeout", c.Proxy.Timeout),
		slog.Int64("max_body_bytes", c.Proxy.MaxBodyBytes),
		slog.Int64("max_response_bytes", c.Upstream.MaxResponseBytes),
		slog.Bool("auth_token_configured", c.Proxy.AuthToken != ""),
		slog.Any("allowed_hosts", c.Proxy.AllowedHosts),
		slog.String("ca_cert", c.Upstream.CACert),
		slog.Bool("insecure_tls", c.Upstream.Insecure),
		slog.Bool("raw", c.Proxy.Raw),
		slog.String("log_level", strings.ToLower(c.LogLevel.String())),
		slog.String("log_format", c.LogFormat),
	)
}
