package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/joho/godotenv"
)

// envOptions is the single place that ties every flag to its environment
// variables. When several are listed the first non-empty one wins. A flag given
// on the command line always overrides its environment variables.
//
// -healthcheck is deliberately absent: it selects a mode of operation rather
// than a setting, and an inherited variable must not turn the server into a probe.
var envOptions = []struct {
	flag string
	env  []string
}{
	{"listen", []string{"FIM_LISTEN"}},
	{"upstream", []string{"CORP_API_URL", "FIM_UPSTREAM_URL"}},
	{"api-key", []string{"CORP_API_KEY", "FIM_API_KEY"}},
	{"auth-header", []string{"FIM_AUTH_HEADER"}},
	{"header", []string{"FIM_HEADERS"}},
	{"model", []string{"CORP_API_MODEL", "FIM_MODEL"}},
	{"max-tokens-field", []string{"FIM_MAX_TOKENS_FIELD"}},
	{"max-tokens", []string{"FIM_MAX_TOKENS"}},
	{"temperature", []string{"FIM_TEMPERATURE"}},
	{"extra-body", []string{"FIM_EXTRA_BODY"}},
	{"send-stop", []string{"FIM_SEND_STOP"}},
	{"timeout", []string{"FIM_TIMEOUT"}},
	{"max-body-bytes", []string{"FIM_MAX_BODY_BYTES"}},
	{"max-response-bytes", []string{"FIM_MAX_RESPONSE_BYTES"}},
	{"auth-token", []string{"FIM_AUTH_TOKEN"}},
	{"allowed-hosts", []string{"FIM_ALLOWED_HOSTS"}},
	{"ca-cert", []string{"FIM_CA_CERT"}},
	{"insecure", []string{"FIM_INSECURE"}},
	{"system-prompt-file", []string{"FIM_SYSTEM_PROMPT_FILE"}},
	{"raw", []string{"FIM_RAW"}},
	{"log-level", []string{"FIM_LOG_LEVEL"}},
	{"log-format", []string{"FIM_LOG_FORMAT"}},
	// Deprecated, kept so existing scripts and .env files keep working.
	{"debug", []string{"FIM_DEBUG"}},
	{"v", []string{"FIM_VERBOSE"}},
}

func envNames(flagName string) []string {
	for _, o := range envOptions {
		if o.flag == flagName {
			return o.env
		}
	}
	return nil
}

// LoadDotEnv copies entries of ./.env into the process environment, without
// overriding variables that are already set. A missing file is fine. Call it
// before Parse.
func LoadDotEnv() error {
	err := godotenv.Load(".env")
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr
	}
	// Parser errors may include the line containing a credential.
	return errors.New("invalid .env syntax")
}

// Docker's --env-file preserves surrounding quotes (Compose strips them), so
// accept both forms for every environment value.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// firstEnv returns the first non-empty variable among names.
func firstEnv(names []string) (name, value string) {
	for _, n := range names {
		if v := unquote(os.Getenv(n)); v != "" {
			return n, v
		}
	}
	return "", ""
}

// applyEnv sets every flag that was not given on the command line from its
// environment variables, using the flag's own parser so typed values
// (durations, numbers, booleans) and repeated headers behave identically to
// the command line. If only is non-empty, just those flags are considered.
// Error messages name the variable but never echo its value, which may be a key.
func applyEnv(flagSet *flag.FlagSet, only ...string) error {
	explicit := explicitFlags(flagSet)
	for _, option := range envOptions {
		if explicit[option.flag] || (len(only) > 0 && !slices.Contains(only, option.flag)) {
			continue
		}
		envName, value := firstEnv(option.env)
		if value == "" {
			continue
		}
		items := []string{value}
		if option.flag == "header" {
			items = strings.Split(value, ";")
		}
		for _, item := range items {
			if item = strings.TrimSpace(item); item == "" {
				continue
			}
			if err := flagSet.Set(option.flag, item); err != nil {
				return fmt.Errorf("invalid value for %s (flag -%s)", envName, option.flag)
			}
		}
	}
	return nil
}

// explicitFlags returns the names of the flags set so far (on the command line,
// or by an earlier flagSet.Set).
func explicitFlags(flagSet *flag.FlagSet) map[string]bool {
	set := make(map[string]bool)
	flagSet.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}
