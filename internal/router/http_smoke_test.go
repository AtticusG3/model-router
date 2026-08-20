package router

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-router/internal/config"
)

func TestHTTPServerPeerRouting(t *testing.T) {
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "router-a")
	h := NewHandler(r, NewLogger(io.Discard, false))

	body := `{"model":"digger/remote-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "no model id could be identified") {
		t.Fatalf("matcher failed: %s", rec.Body.String())
	}
	t.Logf("status=%d body=%s", rec.Code, rec.Body.String())
}

func TestHTTPServerWebUI(t *testing.T) {
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	h := NewHandler(r, logger)

	req := httptest.NewRequest("GET", "/ui/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("UI status = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "model-router") {
		t.Fatalf("UI response missing HTML shell: content-type=%q", rec.Header().Get("Content-Type"))
	}

	req = httptest.NewRequest("GET", "/_router/logs", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "entries") {
		t.Fatalf("logs status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHTTPServerWebUIModelQuery(t *testing.T) {
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "router-a")
	h := NewHandler(r, NewLogger(io.Discard, false))

	// A native llama.cpp WebUI endpoint must reach model matching instead of
	// being rejected by the router's catch-all 404.
	req := httptest.NewRequest("GET", "/props?model=not-a-model", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "no model id could be identified") {
		t.Fatalf("WebUI request did not reach matcher: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHTTPServerBodyModel(t *testing.T) {
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "router-a")
	h := NewHandler(r, NewLogger(io.Discard, false))

	// agents-a1 stanza exists but its fake backend won't be running in tests;
	// we only assert the matcher resolves it (spawn will fail -> 503, not 404).
	body := `{"model":"agents-a1","messages":[]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "no model id could be identified") {
		t.Fatalf("matcher failed: %s", rec.Body.String())
	}
	t.Logf("status=%d body=%s", rec.Code, rec.Body.String())
}
