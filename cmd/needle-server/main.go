// Command needle-server ingests reports from needle-client agents, keeps the
// recent history in memory-mapped SQLite, and serves a small dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	needle "needle"
	"needle/internal/server"
	"needle/internal/store"
)

var version = "dev"

type config struct {
	addr          string
	db            string
	agentToken    string
	password      string
	sessionSecret string
	secureCookies bool
	rollupEvery   time.Duration
	verbose       bool
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "needle-server:", err)
		os.Exit(2)
	}
	if cfg.agentToken == "" {
		fmt.Fprintln(os.Stderr, "needle-server: agent token is required (--agent-token or NEEDLE_AGENT_TOKEN)")
		os.Exit(2)
	}

	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	st, err := store.Open(cfg.db)
	if err != nil {
		slog.Error("cannot open database", "path", cfg.db, "err", err)
		os.Exit(1)
	}
	defer st.Close()

	web, err := fs.Sub(needle.WebFS, "web")
	if err != nil {
		slog.Error("embedded web assets are missing", "err", err)
		os.Exit(1)
	}

	handler := server.New(st, web, server.Config{
		AgentToken:    cfg.agentToken,
		Password:      cfg.password,
		SessionSecret: cfg.sessionSecret,
		SecureCookies: cfg.secureCookies,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go maintain(ctx, st, cfg.rollupEvery)

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		slog.Info("needle-server listening",
			"addr", cfg.addr,
			"db", cfg.db,
			"version", version,
			"dashboard_auth", cfg.password != "",
			"agent_token", server.TruncateForLog(cfg.agentToken),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown timed out", "err", err)
	}
}

// maintain rolls up and prunes on a fixed cadence. Rollups only touch closed
// buckets, so the exact moment this runs does not affect the result.
func maintain(ctx context.Context, st *store.Store, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		run := func() {
			now := time.Now().UnixMilli()
			if err := st.Rollup(now); err != nil {
				slog.Error("rollup failed", "err", err)
				return
			}
			if err := st.Prune(now); err != nil {
				slog.Error("prune failed", "err", err)
			}
		}
		run()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func parseFlags(args []string) (config, error) {
	cfg := config{
		addr:        envOr("NEEDLE_ADDR", ":8080"),
		db:          envOr("NEEDLE_DB", "needle.db"),
		agentToken:  os.Getenv("NEEDLE_AGENT_TOKEN"),
		password:    os.Getenv("NEEDLE_PASSWORD"),
		rollupEvery: 5 * time.Minute,
		// Cookies default to plain HTTP so that a local run works out of the
		// box; the container image sets this to true behind a TLS proxy.
		secureCookies: envBool("NEEDLE_SECURE_COOKIES", false),
	}
	cfg.sessionSecret = os.Getenv("NEEDLE_SESSION_SECRET")

	fs := flag.NewFlagSet("needle-server", flag.ContinueOnError)
	fs.StringVar(&cfg.addr, "addr", cfg.addr, "listen address")
	fs.StringVar(&cfg.db, "db", cfg.db, "sqlite database path")
	fs.StringVar(&cfg.agentToken, "agent-token", cfg.agentToken, "shared secret agents must present (NEEDLE_AGENT_TOKEN)")
	fs.StringVar(&cfg.password, "password", cfg.password, "dashboard password; empty disables dashboard auth (NEEDLE_PASSWORD)")
	fs.StringVar(&cfg.sessionSecret, "session-secret", cfg.sessionSecret, "cookie signing key (NEEDLE_SESSION_SECRET)")
	fs.BoolVar(&cfg.secureCookies, "secure-cookies", cfg.secureCookies, "mark the session cookie Secure (set behind HTTPS)")
	fs.DurationVar(&cfg.rollupEvery, "rollup-every", cfg.rollupEvery, "aggregate and prune interval")
	fs.BoolVar(&cfg.verbose, "v", false, "verbose logging")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if !strings.Contains(cfg.addr, ":") {
		cfg.addr = ":" + cfg.addr
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}
