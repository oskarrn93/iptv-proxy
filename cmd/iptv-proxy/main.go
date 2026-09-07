package main

// iptv-proxy: hosts multiple M3U8 IPTV feeds behind unguessable-credential
// proxies, one listen port per feed, all from a single process/container.
//
// Each feed rewrites every playlist/segment URL in the upstream M3U8 so the
// real upstream (and any Xtream credentials embedded in it) never reaches
// the client; clients instead authenticate with their own username/password
// per feed, configured in config.yaml.
//
// Usage:
//
//	go run ./cmd/iptv-proxy -config config.yaml

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/oskarrn93/media-services/iptv-proxy/internal/iptvproxy"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	cfg, err := iptvproxy.LoadConfig(*configPath)
	if err != nil {
		logger.Error("config error", "error", err)
		return 1
	}

	// A bad or missing value defaults to enabled and just logs, rather than
	// exiting: metrics are diagnostic, not load-bearing, and shouldn't be able
	// to keep the proxy from starting.
	metricsEnabled := true
	if raw, ok := os.LookupEnv("METRICS_ENABLED"); ok {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			logger.Error("invalid METRICS_ENABLED value, defaulting to enabled", "value", raw, "error", err)
		} else {
			metricsEnabled = parsed
		}
	}

	servers := make([]*http.Server, 0, len(cfg.Feeds))
	// A feed that cannot bind (port already taken, malformed listen_addr) must
	// bring the process down: leaving it up would keep the container "healthy"
	// while that feed serves nothing.
	serveErrs := make(chan error, len(cfg.Feeds))
	var wg sync.WaitGroup

	for _, feedCfg := range cfg.Feeds {
		proxy, err := iptvproxy.NewFeedProxy(feedCfg, logger, metricsEnabled)
		if err != nil {
			logger.Error("feed setup failed", "feed", feedCfg.Name, "error", err)
			return 1
		}

		srv := &http.Server{
			Addr:        feedCfg.ListenAddr,
			Handler:     proxy.Mux(),
			ReadTimeout: 10 * time.Second,
			// No WriteTimeout: a live TS relay writes for as long as the viewer
			// watches, and any deadline here would cut it off mid-stream.
			WriteTimeout: 0,
			IdleTimeout:  120 * time.Second,
		}
		servers = append(servers, srv)

		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("listening", "feed", feedCfg.Name, "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrs <- fmt.Errorf("feed %q: %w", feedCfg.Name, err)
			}
		}()
	}

	if cfg.HealthAddr != "" {
		aggregator, err := iptvproxy.NewHealthAggregator(cfg.Feeds)
		if err != nil {
			logger.Error("health aggregator setup failed", "error", err)
			return 1
		}
		srv := &http.Server{
			Addr:        cfg.HealthAddr,
			Handler:     aggregator,
			ReadTimeout: 10 * time.Second,
		}
		servers = append(servers, srv)

		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("listening", "feed", "health", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrs <- fmt.Errorf("health aggregator: %w", err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	exitCode := 0
	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", "signal", sig.String())
	case err := <-serveErrs:
		logger.Error("fatal error, shutting down", "error", err)
		exitCode = 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("shutdown error", "error", err)
		}
	}

	wg.Wait()
	return exitCode
}
