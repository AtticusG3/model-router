// model-router is the backend-agnostic model router described in SPEC.md.
// One binary runs on every node; per-node behaviour comes from the YAML config.
//
// Process-supervision patterns (spawn, log capture, crash/restart, health-check
// polling, port allocation) are adapted from mostlygeek/llama-swap. Where code
// is copied directly rather than reimplemented, the source file carries a
// comment crediting llama-swap and its license.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"model-router/internal/config"
	"model-router/internal/router"
)

func main() {
	var (
		configPath = flag.String("config", "/opt/ai/config/model-router.yaml", "path to the model-router YAML config")
		listen     = flag.String("listen", "", "override the listen address (default: config value)")
		node       = flag.String("node", "", "override the node name (default: hostname)")
		verbose    = flag.Bool("verbose", false, "verbose logging")
		check      = flag.Bool("check", false, "validate the config and exit (no server, no preload)")
	)
	flag.Parse()

	logger := router.NewLogger(os.Stdout, *verbose)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Errorf("config: %v", err)
		os.Exit(1)
	}

	if *check {
		fmt.Printf("config OK: %s\n", *configPath)
		fmt.Printf("  listen=%s start_port=%d stanzas=%d pools=%d peers=%d preload=%v\n",
			cfg.Listen, cfg.StartPort, len(cfg.Stanzas), len(cfg.Pools), len(cfg.Peers), cfg.Preload)
		for _, s := range cfg.Stanzas {
			flags := ""
			if s.Match.PathPrefix != "" {
				flags = fmt.Sprintf(" path=%s default=%v", s.Match.PathPrefix, s.Match.PathDefault)
			}
			fmt.Printf("  %-28s port=%-5d vram=%-6d dev=%-4s api=%-9s ttl=%-4d unlisted=%v%s\n",
				s.ModelID, s.Port, s.VramMB, s.Device, s.APIType, s.IdleTTLSeconds, s.Unlisted, flags)
		}
		for name, p := range cfg.Pools {
			fmt.Printf("  pool %-18s spillover=%d targets=%v\n", name, p.Spillover, p.Targets)
		}
		for _, p := range cfg.Peers {
			fmt.Printf("  peer %-18s kind=%-6s url=%s models=%d\n", p.Name, p.Kind, p.BaseURL, len(p.Models))
		}
		return
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	nodeName := *node
	if nodeName == "" {
		nodeName, _ = os.Hostname()
	}

	r := router.New(cfg, logger, nodeName)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// GPU telemetry poller. Poll once synchronously so preload's admission
	// control sees the real GPU state at startup.
	pollEvery := time.Duration(cfg.Telemetry.PollSeconds) * time.Second
	poller := router.NewGPUPoller(r.Ledger(), pollEvery)
	poller.PollOnce()
	go poller.Run(ctx)

	// Peer telemetry syncer.
	syncEvery := time.Duration(cfg.Telemetry.PeerSyncSeconds) * time.Second
	go router.NewPeerSyncer(cfg, r.Peers(), syncEvery).Run(ctx)

	handler := router.NewHandler(r, logger)
	srv, _, errCh, err := startHTTPServer(cfg.Listen, handler, logger)
	if err != nil {
		logger.Errorf("server: %v", err)
		os.Exit(1)
	}

	// Start the HTTP surface before preloading so operators can watch status,
	// metrics, and logs while slow models are starting or failing.
	preloadDone := runPreload(func() {
		logger.Infof("preload starting (%d models)", len(cfg.Preload))
		r.Preload()
		logger.Infof("preload complete")
	})

	// Signal handling for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Errorf("server: %v", err)
			os.Exit(1)
		}
	case sig := <-sigCh:
		logger.Infof("received %s, shutting down", sig)
	}

	// Graceful: stop all managed backends.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	cancel()
	<-preloadDone
	for _, id := range cfg.StanzaIDs() {
		r.Unload(id)
	}
	fmt.Fprintln(os.Stderr, "shutdown complete")
}

func startHTTPServer(addr string, handler http.Handler, logger *router.Logger) (*http.Server, net.Listener, <-chan error, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, nil, err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	errCh := make(chan error, 1)
	logger.Infof("model-router listening on %s", addr)
	go func() {
		errCh <- srv.Serve(listener)
	}()
	return srv, listener, errCh, nil
}

func runPreload(preload func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		preload()
	}()
	return done
}
