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
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/oskarrn93/media-services/iptv-proxy/internal/iptvproxy"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	cfg, err := iptvproxy.LoadConfig(*configPath)
	if err != nil {
		log.Printf("config error: %v", err)
		return 1
	}

	servers := make([]*http.Server, 0, len(cfg.Feeds))
	// A feed that cannot bind (port already taken, malformed listen_addr) must
	// bring the process down: leaving it up would keep the container "healthy"
	// while that feed serves nothing.
	serveErrs := make(chan error, len(cfg.Feeds))
	var wg sync.WaitGroup

	for _, feedCfg := range cfg.Feeds {
		proxy, err := iptvproxy.NewFeedProxy(feedCfg)
		if err != nil {
			log.Printf("feed %q: %v", feedCfg.Name, err)
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
			log.Printf("[%s] listening on %s", feedCfg.Name, srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrs <- fmt.Errorf("feed %q: %w", feedCfg.Name, err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	exitCode := 0
	select {
	case sig := <-sigCh:
		log.Printf("received signal %s, shutting down", sig)
	case err := <-serveErrs:
		log.Printf("fatal: %v, shutting down", err)
		exitCode = 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}

	wg.Wait()
	return exitCode
}
