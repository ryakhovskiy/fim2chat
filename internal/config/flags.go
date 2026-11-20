package config

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"ryakhovskiy/fim2chat/internal/logging"
	"ryakhovskiy/fim2chat/internal/proxy"
	"ryakhovskiy/fim2chat/internal/upstream"
)

// extraOptions holds flag values that need more work than a plain assignment
// (see Config.finish).
type extraOptions struct {
	healthcheck      bool
	allowedHosts     string
	logLevel         string
	debug            bool // deprecated alias of -log-level debug
	verbose          bool // deprecated, no effect
	systemPromptFile string
	extraBodyJSON    string
	headers          headerList
}

// headerList is a repeatable "Name: value" flag.
type headerList []string

func (h *headerList) String() string { return strings.Join(*h, ", ") }

func (h *headerList) Set(v string) error {
	if !strings.Contains(v, ":") {
		return fmt.Errorf("header must look like 'Name: value', got %q", v)
	}
	*h = append(*h, v)
	return nil
}

// help appends a flag's environment variables to its usage text.
func help(flagName, text string) string {
	if names := envNames(flagName); len(names) > 0 {
		return text + " [env: " + strings.Join(names, ", ") + "]"
	}
	return text
}

// register defines every flag. Defaults are fixed values: environment variables
// are applied after parsing (applyEnv), so that help output cannot print a
// secret and a flag can always be told apart from an inherited default.
// Every flag except -healthcheck must have an entry in envOptions.
func register(f *flag.FlagSet, cfg *Config, extra *extraOptions) {
	f.StringVar(&cfg.Listen, "listen", "127.0.0.1:8787", help("listen", "address to listen on"))

	f.StringVar(&cfg.Proxy.AuthToken, "auth-token", "", help("auth-token", "require 'Authorization: Bearer <token>' from the editor (use it as the API key in Zed). Needs -api-key, so the token is never forwarded upstream"))
	f.StringVar(&extra.allowedHosts, "allowed-hosts", "", help("allowed-hosts", "comma-separated host names accepted in the Host header, besides localhost and IP addresses (e.g. a LAN name when serving other machines); '*' disables the check"))

	f.StringVar(&cfg.Upstream.URL, "upstream", "", help("upstream", "full URL of the upstream chat endpoint, e.g. https://host/v1/chat/completions (required)"))
	f.StringVar(&cfg.Upstream.APIKey, "api-key", "", help("api-key", "upstream API key (default: forward the key Zed sends)"))
	f.StringVar(&cfg.Upstream.AuthHeader, "auth-header", "Authorization", help("auth-header", "header for the key: 'Authorization' (Bearer) or e.g. 'api-key' for Azure"))
	f.Var(&extra.headers, "header", help("header", "extra upstream header 'Name: value' (repeatable; in FIM_HEADERS separate several with ';')"))
	f.StringVar(&cfg.Upstream.CACert, "ca-cert", "", help("ca-cert", "PEM file with extra CA certificates (corporate root CA)"))
	f.BoolVar(&cfg.Upstream.Insecure, "insecure", false, help("insecure", "skip TLS certificate verification (last resort)"))
	f.Int64Var(&cfg.Upstream.MaxResponseBytes, "max-response-bytes", upstream.DefaultMaxResponseBytes, help("max-response-bytes", "largest upstream response read; a larger one is logged and fails with 502"))

	f.StringVar(&cfg.Proxy.Model, "model", "", help("model", "upstream model name (default: the model Zed sends)"))
	f.StringVar(&cfg.Proxy.MaxTokensField, "max-tokens-field", "max_tokens", help("max-tokens-field", "'max_tokens' or 'max_completion_tokens' (newer OpenAI models)"))
	f.IntVar(&cfg.Proxy.DefaultMaxTokens, "max-tokens", 256, help("max-tokens", "max tokens when the client sends none"))
	f.Float64Var(&cfg.Proxy.Temperature, "temperature", 0, help("temperature", "sampling temperature; negative = don't send it (needed for some reasoning models)"))
	f.StringVar(&extra.extraBodyJSON, "extra-body", "", help("extra-body", `JSON object merged into the upstream request, e.g. '{"reasoning_effort":"minimal"}'`))
	f.BoolVar(&cfg.Proxy.SendStop, "send-stop", false, help("send-stop", "forward client stop sequences upstream (they are always applied locally)"))
	f.DurationVar(&cfg.Proxy.Timeout, "timeout", 30*time.Second, help("timeout", "upstream request timeout"))
	f.Int64Var(&cfg.Proxy.MaxBodyBytes, "max-body-bytes", proxy.DefaultMaxBodyBytes, help("max-body-bytes", "largest request body accepted from the editor; larger requests are logged and rejected with 413"))
	f.StringVar(&extra.systemPromptFile, "system-prompt-file", "", help("system-prompt-file", "file with a custom FIM system prompt (edit formats keep built-in rewrite instructions)"))
	f.BoolVar(&cfg.Proxy.Raw, "raw", false, help("raw", "disable model output clean-up (edit replies still use no-change for truncated or blank answers)"))
	f.StringVar(&extra.logLevel, "log-level", "", help("log-level", "log verbosity: debug, info, warn or error (default info). debug adds request/response bodies and headers, i.e. your source code"))
	f.StringVar(&cfg.LogFormat, "log-format", logging.FormatText, help("log-format", "log output format: 'text' (key=value) or 'json'"))
	f.BoolVar(&extra.debug, "debug", false, help("debug", "deprecated: use -log-level debug"))
	f.BoolVar(&extra.verbose, "v", false, help("v", "deprecated and ignored: completion text is logged at -log-level debug"))

	f.BoolVar(&extra.healthcheck, "healthcheck", false, "probe /health of the running server and exit 0 (healthy) or 1 (used by Docker HEALTHCHECK); finds the server's -listen address itself")
}
