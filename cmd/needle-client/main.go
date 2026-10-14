// Command needle-client is the agent. It reads host metrics from /proc and
// /sys with the privileges of whoever starts it and POSTs them to a
// needle-server. It holds no inbound socket, accepts no commands and never
// writes to the system, so a compromised server gains nothing from it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"needle/internal/collect"
)

// version is stamped by the build script via -ldflags.
var version = "dev"

type config struct {
	url      string
	token    string
	interval time.Duration
	insecure bool
	once     bool
	verbose  bool
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "needle-client:", err)
		os.Exit(2)
	}

	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if cfg.url == "" || cfg.token == "" {
		fmt.Fprintln(os.Stderr, "needle-client: --url and --token are required (or set NEEDLE_URL / NEEDLE_TOKEN)")
		os.Exit(2)
	}
	if cfg.interval < time.Second {
		cfg.interval = time.Second
	}

	// The agent should be boring to a process monitor: never a CPU hog, and
	// never a reason for the OOM killer to look this way.
	debug.SetMemoryLimit(32 << 20)
	debug.SetGCPercent(40)

	col := collect.New()
	host := col.Host()

	client := &http.Client{
		Timeout: cfg.interval,
		Transport: &http.Transport{
			MaxIdleConns:        2,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			DisableCompression:  false,
		},
	}

	reporter := &reporter{cfg: cfg, client: client, col: col}

	slog.Info("needle-client starting",
		"version", version,
		"server", cfg.url,
		"interval", cfg.interval.String(),
		"host_id", host.ID,
		"host", host.Name,
	)

	if cfg.once {
		if err := reporter.runOnce(); err != nil {
			slog.Error("report failed", "err", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	reporter.loop(ctx)
	slog.Info("needle-client stopped")
}

func parseFlags(args []string) (config, error) {
	cfg := config{
		url:      envOr("NEEDLE_URL", ""),
		token:    envOr("NEEDLE_TOKEN", ""),
		interval: 30 * time.Second,
	}
	if v := os.Getenv("NEEDLE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.interval = d
		}
	}

	fs := flag.NewFlagSet("needle-client", flag.ContinueOnError)
	fs.StringVar(&cfg.url, "url", cfg.url, "server base url, e.g. https://needle.example.com")
	fs.StringVar(&cfg.token, "token", cfg.token, "agent token (NEEDLE_TOKEN)")
	fs.DurationVar(&cfg.interval, "interval", cfg.interval, "report interval")
	fs.BoolVar(&cfg.insecure, "insecure", false, "skip TLS verification (testing only)")
	fs.BoolVar(&cfg.once, "once", false, "collect and report a single sample, then exit")
	fs.BoolVar(&cfg.verbose, "v", false, "verbose logging")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if *showVersion {
		fmt.Println("needle-client", version)
		os.Exit(0)
	}
	cfg.url = strings.TrimRight(cfg.url, "/")
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// reporter owns the collection loop and the retry behaviour around a failed
// POST. A single failed report is dropped rather than queued: the dashboard
// is a live view, and replaying stale samples after an outage would draw a
// misleading history.
type reporter struct {
	cfg    config
	client *http.Client
	col    *collect.Collector
}

func (r *reporter) loop(ctx context.Context) {
	if err := r.col.Prime(); err != nil {
		slog.Warn("no baseline counters; first rates will read zero", "err", err)
	}

	timer := time.NewTimer(r.cfg.interval)
	defer timer.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := r.report(ctx); err != nil {
			failures++
			backoff := r.cfg.interval
			if failures > 2 {
				backoff = min(r.cfg.interval*time.Duration(failures-1), 10*time.Minute)
			}
			slog.Warn("report failed", "err", err, "consecutive", failures, "retry_in", backoff.String())
			timer.Reset(backoff)
			continue
		}
		if failures > 0 {
			slog.Info("report recovered", "after_failures", failures)
			failures = 0
		}
		timer.Reset(r.cfg.interval)
	}
}

func (r *reporter) runOnce() error {
	if err := r.col.Prime(); err != nil {
		slog.Warn("no baseline counters; rates will read zero", "err", err)
	}
	time.Sleep(1100 * time.Millisecond)
	return r.report(context.Background())
}

func (r *reporter) report(ctx context.Context) error {
	rep := r.col.Collect()
	rep.Interval = int(r.cfg.interval.Seconds())
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}

	url := r.cfg.url + "/report"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.cfg.token)
	req.Header.Set("User-Agent", "needle-client/"+version)

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused by the next report.
	_, _ = resp.Body.Read(make([]byte, 512))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	slog.Debug("reported",
		"cpu", rep.Sample.CPU,
		"series", len(rep.Series),
		"bytes", len(body),
	)
	return nil
}
