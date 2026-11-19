// Command fim2chat is a proxy that turns Zed's legacy fill-in-the-middle
// /v1/completions requests into chat-completion requests for an upstream.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ryakhovskiy/fim2chat/internal/config"
	"ryakhovskiy/fim2chat/internal/health"
	"ryakhovskiy/fim2chat/internal/logging"
	"ryakhovskiy/fim2chat/internal/proxy"
	"ryakhovskiy/fim2chat/internal/upstream"
)

func main() {
	// A plain logger until the configured format is known, so that
	// configuration errors are reported in a readable way too.
	_, _ = logging.Setup(os.Stderr, logging.FormatText, slog.LevelInfo)
	if err := run(); err != nil {
		slog.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	if err := config.LoadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.Parse(flag.CommandLine, os.Args[1:])
	if errors.Is(err, config.ErrMissingUpstream) {
		fmt.Fprintln(os.Stderr, "error:", err)
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		return err
	}
	if cfg.Healthcheck {
		// A probe is not a service: keep its output to the failure reason.
		if err := health.Probe(cfg.Listen); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if _, err := logging.Setup(os.Stderr, cfg.LogFormat, cfg.LogLevel); err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		slog.Warn(w)
	}
	slog.Info("starting fim2chat", "config", cfg)
	return serve(cfg)
}

func serve(cfg config.Config) error {
	client, err := upstream.New(cfg.Upstream)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           proxy.NewHandler(cfg.Proxy, client),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Must outlast the upstream call, or slow answers are cut off mid-write.
		WriteTimeout: cfg.Proxy.Timeout + 15*time.Second,
		IdleTimeout:  2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	slog.Info("listening", "address", cfg.Listen, "endpoint", "http://"+cfg.Listen+"/v1/completions", "upstream", logging.URLString(cfg.Upstream.URL))

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutdown signal received, draining in-flight requests", "grace_period", "10s")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("stopped")
	return nil
}
