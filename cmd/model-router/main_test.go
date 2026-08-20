package main

import (
	"io"
	"net/http"
	"testing"
	"time"

	"model-router/internal/config"
	"model-router/internal/router"
)

func TestHTTPServerAvailableWhilePreloadRuns(t *testing.T) {
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	logger := router.NewLogger(io.Discard, false)
	r := router.New(cfg, logger, "router-a")
	srv, listener, errCh, err := startHTTPServer("127.0.0.1:0", router.NewHandler(r, logger), logger)
	if err != nil {
		t.Fatalf("start HTTP server: %v", err)
	}
	defer srv.Close()

	preloadStarted := make(chan struct{})
	preloadRelease := make(chan struct{})
	preloadDone := runPreload(func() {
		close(preloadStarted)
		<-preloadRelease
	})
	<-preloadStarted

	client := &http.Client{Timeout: time.Second}
	for _, path := range []string{"/metrics", "/_router/logs", "/ui/"} {
		resp, err := client.Get("http://" + listener.Addr().String() + path)
		if err != nil {
			t.Fatalf("%s unavailable while preload runs: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status while preload runs = %d", path, resp.StatusCode)
		}
	}

	close(preloadRelease)
	select {
	case <-preloadDone:
	case <-time.After(time.Second):
		t.Fatal("preload did not finish")
	}

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			t.Fatalf("HTTP server: %v", err)
		}
	default:
	}
}
