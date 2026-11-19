package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
)

// resolveHealthcheckListen finds the address the probe should call. Docker runs
// the probe without the server's command line, so the order is: the probe's own
// -listen, the -listen of the server process, FIM_LISTEN, the default.
func resolveHealthcheckListen(flagSet *flag.FlagSet, cfg *Config) error {
	if explicitFlags(flagSet)["listen"] {
		return nil
	}
	if l := listenFromProc("/proc/1/cmdline"); l != "" {
		cfg.Listen = l
		return nil
	}
	return applyEnv(flagSet, "listen")
}

// listenFromProc returns the -listen value on the command line of the process
// in path (PID 1 inside a container), or "" if it cannot be determined or the
// process is not this program.
func listenFromProc(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	self := filepath.Base(os.Args[0])
	for _, a := range args {
		if filepath.Base(a) == self {
			return listenFromArgs(args)
		}
	}
	return ""
}

// listenFromArgs finds the last -listen / --listen value, as the flag package would.
func listenFromArgs(args []string) string {
	listen := ""
	for i := 0; i < len(args); i++ {
		a := strings.TrimPrefix(strings.TrimPrefix(args[i], "-"), "-")
		if a == args[i] {
			continue
		}
		if a == "listen" && i+1 < len(args) {
			listen = args[i+1]
			i++
		} else if v, ok := strings.CutPrefix(a, "listen="); ok {
			listen = v
		}
	}
	return listen
}
