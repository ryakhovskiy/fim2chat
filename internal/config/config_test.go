package config

import (
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseConfig(t *testing.T) {
	t.Setenv("CORP_API_URL", "")
	t.Setenv("FIM_UPSTREAM_URL", "https://example.test/chat")
	t.Setenv("FIM_MODEL", "from-env")
	t.Setenv("FIM_EXTRA_BODY", `{"seed":12}`)
	flags := flag.NewFlagSet("fim2chat", flag.ContinueOnError)
	cfg, err := Parse(flags, []string{"-log-level", "debug", "-model", "override", "-header", "X-One: first", "-header", "X-Two: second"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.URL != "https://example.test/chat" || !slices.Equal(cfg.Upstream.Headers, []string{"X-One: first", "X-Two: second"}) {
		t.Error("upstream URL or extra headers lost during configuration")
	}
	if cfg.Proxy.Model != "override" || cfg.Proxy.ExtraBody["seed"] != float64(12) {
		t.Error("proxy model override or extra JSON fields lost during configuration")
	}
	if cfg.LogLevel != slog.LevelDebug || len(cfg.Warnings) != 0 {
		t.Errorf("-log-level debug: level=%v warnings=%v", cfg.LogLevel, cfg.Warnings)
	}
}

func TestCorporateAPIConfigPrecedence(t *testing.T) {
	t.Setenv("FIM_UPSTREAM_URL", "https://legacy.example/chat")
	t.Setenv("FIM_API_KEY", "legacy-key")
	t.Setenv("FIM_MODEL", "legacy-model")
	t.Setenv("FIM_EXTRA_BODY", "")

	for _, tc := range []struct {
		name, url, key, model       string
		args                        []string
		wantURL, wantKey, wantModel string
	}{
		{name: "legacy fallback", wantURL: "https://legacy.example/chat", wantKey: "legacy-key", wantModel: "legacy-model"},
		{name: "docker env file", url: "https://corp.example/chat", key: "corp-key", model: "@personal-openai/gpt-6-luna",
			wantURL: "https://corp.example/chat", wantKey: "corp-key", wantModel: "@personal-openai/gpt-6-luna"},
		{name: "older quoted env file", url: `"https://corp.example/chat"`, key: "'corp-key'", model: `"@personal-openai/gpt-6-luna"`,
			wantURL: "https://corp.example/chat", wantKey: "corp-key", wantModel: "@personal-openai/gpt-6-luna"},
		{name: "command line", url: "https://corp.example/chat", key: "corp-key", model: "corp-model",
			args:    []string{"-upstream", "https://flag.example/chat", "-api-key", "flag-key", "-model", "flag-model"},
			wantURL: "https://flag.example/chat", wantKey: "flag-key", wantModel: "flag-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CORP_API_URL", tc.url)
			t.Setenv("CORP_API_KEY", tc.key)
			t.Setenv("CORP_API_MODEL", tc.model)
			cfg, err := Parse(flag.NewFlagSet("test", flag.ContinueOnError), tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Upstream.URL != tc.wantURL || cfg.Upstream.APIKey != tc.wantKey || cfg.Proxy.Model != tc.wantModel {
				t.Error("unexpected upstream URL, key, or model")
			}
		})
	}
}

func TestQuotedEnvValuesAreAccepted(t *testing.T) {
	t.Setenv("CORP_API_URL", "https://gw.example/v1/chat/completions")
	t.Setenv("FIM_EXTRA_BODY", `'{"reasoning_effort":"none"}'`)
	cfg, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy.ExtraBody["reasoning_effort"] != "none" {
		t.Fatalf("extra body not parsed: %v", cfg.Proxy.ExtraBody)
	}
}

func TestHealthcheckDoesNotRequireUpstream(t *testing.T) {
	t.Setenv("CORP_API_URL", "")
	t.Setenv("FIM_UPSTREAM_URL", "")
	cfg, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), []string{"-healthcheck"})
	if err != nil || !cfg.Healthcheck {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestDotEnvStartup(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := LoadDotEnv(); err != nil {
		t.Fatalf("missing optional .env: %v", err)
	}
	t.Setenv("CORP_API_URL", "https://injected.example/chat")
	t.Setenv("CORP_API_MODEL", "injected-model")
	// Allow dotenv to set these keys, then restore the caller's environment.
	for _, key := range []string{"FIM_TEMPERATURE", "FIM_HEADERS"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	const contents = "export CORP_API_URL=https://file.example/chat\nCORP_API_MODEL=file-model\nFIM_TEMPERATURE=-1\nFIM_HEADERS='X-One: first; X-Two: second'\n"
	if err := os.WriteFile(".env", []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadDotEnv(); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(flag.NewFlagSet("test", flag.ContinueOnError), []string{"-model", "flag-model"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.URL != "https://injected.example/chat" || cfg.Proxy.Model != "flag-model" || cfg.Proxy.Temperature != -1 || !slices.Equal(cfg.Upstream.Headers, []string{"X-One: first", "X-Two: second"}) {
		t.Error("expected injected URL, flag model, and dotenv temperature and headers")
	}
}

func TestMalformedDotEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("CORP_API_KEY='unterminated-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadDotEnv(); err == nil || strings.Contains(err.Error(), "unterminated-secret") {
		t.Fatal("malformed dotenv should fail without exposing the input")
	}
}

// Every setting can come from the environment, and a command-line flag wins.
func TestEveryOptionHasAnEnvVar(t *testing.T) {
	dir := t.TempDir()
	promptFile := dir + "/prompt.txt"
	if err := os.WriteFile(promptFile, []byte("custom prompt"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, o := range envOptions {
		for _, name := range o.env {
			t.Setenv(name, "")
		}
	}
	env := map[string]string{
		"FIM_LISTEN": "0.0.0.0:9", "CORP_API_URL": "https://e.example/chat", "CORP_API_KEY": "k",
		"FIM_AUTH_HEADER": "api-key", "FIM_HEADERS": "X-A: 1; X-B: 2;", "CORP_API_MODEL": "m",
		"FIM_MAX_TOKENS_FIELD": "max_completion_tokens", "FIM_MAX_TOKENS": "77", "FIM_TEMPERATURE": "0.5",
		"FIM_EXTRA_BODY": `{"a":1}`, "FIM_SEND_STOP": "true", "FIM_TIMEOUT": "5s",
		"FIM_MAX_BODY_BYTES": "1234", "FIM_MAX_RESPONSE_BYTES": "5678", "FIM_CA_CERT": "/x.pem",
		"FIM_INSECURE": "true", "FIM_SYSTEM_PROMPT_FILE": promptFile, "FIM_RAW": "true",
		"FIM_LOG_LEVEL": "warn", "FIM_LOG_FORMAT": "json",
		"FIM_AUTH_TOKEN": "env-token-long-enough", "FIM_ALLOWED_HOSTS": "Proxy.Corp.Example:8787, other.example ,,",
	}
	seen := map[string]bool{}
	for _, o := range envOptions {
		seen[o.flag] = true
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
	cfg, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), nil)
	if err != nil {
		t.Fatal(err)
	}
	p, u := cfg.Proxy, cfg.Upstream
	switch {
	case cfg.Listen != "0.0.0.0:9", u.URL != "https://e.example/chat", u.APIKey != "k", u.AuthHeader != "api-key",
		!slices.Equal(u.Headers, []string{"X-A: 1", "X-B: 2"}), p.Model != "m", p.MaxTokensField != "max_completion_tokens",
		p.DefaultMaxTokens != 77, p.Temperature != 0.5, p.ExtraBody["a"] != float64(1), !p.SendStop,
		p.Timeout != 5*time.Second, p.MaxBodyBytes != 1234, u.MaxResponseBytes != 5678, u.CACert != "/x.pem",
		!u.Insecure, p.SystemPrompt != "custom prompt", !p.Raw, cfg.LogLevel != slog.LevelWarn, cfg.LogFormat != "json",
		p.AuthToken != "env-token-long-enough", !slices.Equal(p.AllowedHosts, []string{"proxy.corp.example", "other.example"}):
		t.Errorf("environment not fully applied: %+v", cfg)
	}

	// Every registered flag must be covered by the table above.
	flags := flag.NewFlagSet("t", flag.ContinueOnError)
	if _, err := Parse(flags, nil); err != nil {
		t.Fatal(err)
	}
	flags.VisitAll(func(f *flag.Flag) {
		if f.Name != "healthcheck" && !seen[f.Name] {
			t.Errorf("flag -%s has no environment variable", f.Name)
		}
	})
	for flagName := range seen {
		if flags.Lookup(flagName) == nil {
			t.Errorf("envOptions names unknown flag -%s", flagName)
		}
	}

	// Command-line flags override the environment, option by option.
	cfg, err = Parse(flag.NewFlagSet("t", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:1", "-max-tokens", "5", "-timeout", "1s", "-max-body-bytes", "9",
		"-max-response-bytes", "8", "-temperature", "-1", "-send-stop=false", "-raw=false", "-log-level", "error",
		"-auth-token", "flag-token-long-enough", "-allowed-hosts", "flag.example",
		"-header", "X-C: 3", "-auth-header", "Authorization", "-max-tokens-field", "max_tokens", "-log-format", "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	p, u = cfg.Proxy, cfg.Upstream
	switch {
	case cfg.Listen != "127.0.0.1:1", p.DefaultMaxTokens != 5, p.Timeout != time.Second, p.MaxBodyBytes != 9,
		u.MaxResponseBytes != 8, p.Temperature != -1, p.SendStop, p.Raw, cfg.LogLevel != slog.LevelError, p.AuthToken != "flag-token-long-enough", !slices.Equal(p.AllowedHosts, []string{"flag.example"}),
		!slices.Equal(u.Headers, []string{"X-C: 3"}), u.AuthHeader != "Authorization", p.MaxTokensField != "max_tokens", cfg.LogFormat != "text":
		t.Errorf("flags did not override the environment: %+v", cfg)
	}
}

func TestInvalidEnvValueIsReportedWithoutLeakingIt(t *testing.T) {
	t.Setenv("CORP_API_URL", "https://e.example/chat")
	t.Setenv("FIM_TIMEOUT", "soon-secret")
	_, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), nil)
	if err == nil || !strings.Contains(err.Error(), "FIM_TIMEOUT") || strings.Contains(err.Error(), "soon-secret") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSizeLimitsMustBePositive(t *testing.T) {
	t.Setenv("CORP_API_URL", "https://e.example/chat")
	if _, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), []string{"-max-body-bytes", "0"}); err == nil {
		t.Fatal("a zero body limit should be rejected")
	}
}

func TestListenFromArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"/fim2chat", "-listen", ":9000"}, ":9000"},
		{[]string{"/fim2chat", "--listen=:9001", "-v"}, ":9001"},
		{[]string{"/fim2chat", "-listen", ":1", "-listen=:2"}, ":2"},
		{[]string{"/fim2chat", "-upstream", "-listen"}, ""},
		{[]string{"/fim2chat", "-v"}, ""},
	} {
		if got := listenFromArgs(tc.args); got != tc.want {
			t.Errorf("listenFromArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestListenFromProc(t *testing.T) {
	self := filepath.Base(os.Args[0])
	file := t.TempDir() + "/cmdline"
	write := func(args ...string) {
		if err := os.WriteFile(file, []byte(strings.Join(args, "\x00")+"\x00"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("/app/"+self, "-listen", ":9000")
	if got := listenFromProc(file); got != ":9000" {
		t.Errorf("got %q", got)
	}
	write("/sbin/init", "-listen", ":9000") // some other program
	if got := listenFromProc(file); got != "" {
		t.Errorf("foreign process trusted: %q", got)
	}
	if got := listenFromProc(file + ".missing"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestHealthcheckFollowsEnvListen(t *testing.T) {
	t.Setenv("FIM_LISTEN", "0.0.0.0:9999")
	cfg, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), []string{"-healthcheck"})
	// PID 1 of the test machine is not this program, so the env value applies.
	if err != nil || cfg.Listen != "0.0.0.0:9999" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	cfg, _ = Parse(flag.NewFlagSet("t", flag.ContinueOnError), []string{"-healthcheck", "-listen", ":1"})
	if cfg.Listen != ":1" {
		t.Fatalf("flag should win, got %q", cfg.Listen)
	}
}

func TestInvalidLogFormatIsRejected(t *testing.T) {
	t.Setenv("CORP_API_URL", "https://e.example/chat")
	if _, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), []string{"-log-format", "yaml"}); err == nil {
		t.Fatal("unknown log format accepted")
	}
}

// The startup log shows the effective configuration, but never a secret.
func TestConfigLogValueHidesSecrets(t *testing.T) {
	t.Setenv("CORP_API_URL", "https://e.example/chat?api-key=url-secret")
	t.Setenv("CORP_API_KEY", "key-secret")
	t.Setenv("FIM_HEADERS", "x-portkey-config: header-secret; X-Other: 1")
	cfg, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("starting", "config", cfg)
	out := buf.String()
	for _, secret := range []string{"url-secret", "key-secret", "header-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q in startup log: %s", secret, out)
		}
	}
	for _, want := range []string{"config.api_key_configured=true", "config.upstream_url", "x-portkey-config", "config.timeout=30s"} {
		if !strings.Contains(out, want) {
			t.Errorf("startup log lacks %q: %s", want, out)
		}
	}
}

func TestLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		args     []string
		want     slog.Level
		warnings []string // substrings, one per expected warning
	}{
		{name: "default is info", want: slog.LevelInfo},
		{name: "flag", args: []string{"-log-level", "debug"}, want: slog.LevelDebug},
		{name: "env, case-insensitive", env: map[string]string{"FIM_LOG_LEVEL": "WARN"}, want: slog.LevelWarn},
		{name: "warning is accepted as warn", env: map[string]string{"FIM_LOG_LEVEL": "warning"}, want: slog.LevelWarn},
		{name: "flag beats env", env: map[string]string{"FIM_LOG_LEVEL": "error"}, args: []string{"-log-level", "debug"}, want: slog.LevelDebug},
		{name: "quoted env value", env: map[string]string{"FIM_LOG_LEVEL": `"error"`}, want: slog.LevelError},

		// Deprecated switches keep working, with a warning.
		{name: "-debug is an alias of debug", args: []string{"-debug"}, want: slog.LevelDebug, warnings: []string{"-debug / FIM_DEBUG is deprecated"}},
		{name: "FIM_DEBUG=true", env: map[string]string{"FIM_DEBUG": "true"}, want: slog.LevelDebug, warnings: []string{"FIM_DEBUG is deprecated"}},
		{name: "FIM_DEBUG=false still warns, stays info", env: map[string]string{"FIM_DEBUG": "false"}, want: slog.LevelInfo, warnings: []string{"FIM_DEBUG is deprecated"}},
		{name: "-log-level wins over -debug", args: []string{"-debug", "-log-level", "warn"}, want: slog.LevelWarn, warnings: []string{"FIM_DEBUG is deprecated"}},
		{name: "-v is ignored, with a warning", args: []string{"-v"}, want: slog.LevelInfo, warnings: []string{"-v / FIM_VERBOSE is deprecated and has no effect"}},
		{name: "FIM_VERBOSE is ignored, with a warning", env: map[string]string{"FIM_VERBOSE": "true"}, want: slog.LevelInfo, warnings: []string{"FIM_VERBOSE is deprecated and has no effect"}},
		{name: "both deprecated", args: []string{"-v", "-debug"}, want: slog.LevelDebug, warnings: []string{"FIM_DEBUG", "FIM_VERBOSE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CORP_API_URL", "https://e.example/chat")
			for _, name := range []string{"FIM_LOG_LEVEL", "FIM_DEBUG", "FIM_VERBOSE"} {
				t.Setenv(name, tc.env[name])
			}
			cfg, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LogLevel != tc.want {
				t.Errorf("level = %v, want %v", cfg.LogLevel, tc.want)
			}
			if len(cfg.Warnings) != len(tc.warnings) {
				t.Fatalf("warnings = %q, want %d containing %q", cfg.Warnings, len(tc.warnings), tc.warnings)
			}
			for i, want := range tc.warnings {
				if !strings.Contains(cfg.Warnings[i], want) {
					t.Errorf("warning %d = %q, want it to contain %q", i, cfg.Warnings[i], want)
				}
			}
		})
	}
}

func TestInvalidLogLevelIsRejected(t *testing.T) {
	t.Setenv("CORP_API_URL", "https://e.example/chat")
	_, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), []string{"-log-level", "loud"})
	if err == nil || !strings.Contains(err.Error(), "debug, info, warn, error") {
		t.Fatalf("want an error listing the valid levels, got %v", err)
	}
	t.Setenv("FIM_LOG_LEVEL", "verbose")
	_, err = Parse(flag.NewFlagSet("t", flag.ContinueOnError), nil)
	if err == nil {
		t.Fatal("an invalid FIM_LOG_LEVEL must be rejected")
	}
}

func TestAuthTokenRequiresAnUpstreamKey(t *testing.T) {
	t.Setenv("CORP_API_URL", "https://e.example/chat")
	t.Setenv("CORP_API_KEY", "")
	t.Setenv("FIM_API_KEY", "")
	// Without a configured key the editor's credential is forwarded upstream,
	// which would hand the shared token to the upstream service.
	_, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), []string{"-auth-token", "a-sufficiently-long-token"})
	if err == nil || !strings.Contains(err.Error(), "-api-key") || strings.Contains(err.Error(), "a-sufficiently-long-token") {
		t.Fatalf("want an error about -api-key that does not echo the token, got %v", err)
	}
	if _, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), []string{"-auth-token", "a-sufficiently-long-token", "-api-key", "k"}); err != nil {
		t.Fatalf("token with a key must be accepted: %v", err)
	}
}

func TestAccessControlWarnings(t *testing.T) {
	t.Setenv("CORP_API_URL", "https://e.example/chat")
	t.Setenv("CORP_API_KEY", "k")
	for _, tc := range []struct {
		name string
		args []string
		want string // substring of the single expected warning; "" = none
	}{
		{"secure setup", []string{"-auth-token", "a-sufficiently-long-token"}, ""},
		{"no token at all", nil, ""},
		{"short token", []string{"-auth-token", "short"}, "shorter than"},
		{"host check off", []string{"-allowed-hosts", "*"}, "DNS rebinding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if (tc.want == "") != (len(cfg.Warnings) == 0) || (tc.want != "" && !strings.Contains(cfg.Warnings[0], tc.want)) {
				t.Errorf("warnings = %q, want one containing %q", cfg.Warnings, tc.want)
			}
		})
	}
}

func TestStartupLogHidesTheAuthToken(t *testing.T) {
	t.Setenv("CORP_API_URL", "https://e.example/chat")
	t.Setenv("CORP_API_KEY", "k")
	t.Setenv("FIM_AUTH_TOKEN", "shared-token-secret-value")
	t.Setenv("FIM_ALLOWED_HOSTS", "proxy.corp.example")
	cfg, err := Parse(flag.NewFlagSet("t", flag.ContinueOnError), nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("starting", "config", cfg)
	out := buf.String()
	if strings.Contains(out, "shared-token-secret-value") {
		t.Errorf("token in startup log: %s", out)
	}
	for _, want := range []string{"config.auth_token_configured=true", "proxy.corp.example"} {
		if !strings.Contains(out, want) {
			t.Errorf("startup log lacks %q: %s", want, out)
		}
	}
}
