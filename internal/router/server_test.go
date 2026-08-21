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

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	logger := NewLogger(io.Discard, false)
	return NewHandler(New(cfg, logger, "router-a"), logger)
}

func TestHandlerRouteTable(t *testing.T) {
	h := testHandler(t)

	// Prefix /v1/{path...} reaches the matcher (not the catch-all 404 page).
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no model id could be identified") {
		t.Fatalf("/v1/embeddings: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// More-specific /v1/models still lists, not proxy.
	req = httptest.NewRequest("GET", "/v1/models", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"object":"list"`) {
		t.Fatalf("/v1/models: status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "coding-pool") {
		t.Fatalf("/v1/models listed selector: %s", body)
	}
	if strings.Contains(body, "digger/remote-model") {
		t.Fatalf("/v1/models listed peer-qualified id: %s", body)
	}

	// sdapi prefix still model-routes (path_default stanza).
	req = httptest.NewRequest("POST", "/sdapi/v1/txt2img", strings.NewReader(`{"prompt":"cat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "404 page not found") {
		t.Fatalf("sdapi prefix fell through to catch-all: %s", rec.Body.String())
	}

	req = httptest.NewRequest("GET", "/health", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "OK" {
		t.Fatalf("/health: status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "model_router_up") || !strings.Contains(rec.Body.String(), "model_router_model_running") {
		t.Fatalf("/metrics: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerUnloadCallsUnload(t *testing.T) {
	h := testHandler(t)

	req := httptest.NewRequest("POST", "/_router/unload", bytes.NewBufferString(`{"model_id":"agents-a1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not loaded") {
		t.Fatalf("unload: status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest("GET", "/_router/unload", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unload GET: status=%d", rec.Code)
	}
}

func TestSplitUpstreamPath(t *testing.T) {
	cases := []struct {
		path     string
		id       string
		upPath   string
		hadSlash bool
	}{
		{"/upstream", "", "", false},
		{"/upstream/", "", "", false},
		{"/upstream/agents-a1", "agents-a1", "", false},
		{"/upstream/agents-a1/", "agents-a1", "/", true},
		{"/upstream/agents-a1/props", "agents-a1", "/props", true},
		{"/upstream/agents-a1/v1/chat/completions", "agents-a1", "/v1/chat/completions", true},
	}
	for _, tc := range cases {
		id, up, slash := splitUpstreamPath(tc.path)
		if id != tc.id || up != tc.upPath || slash != tc.hadSlash {
			t.Fatalf("%s: id=%q up=%q slash=%v, want id=%q up=%q slash=%v",
				tc.path, id, up, slash, tc.id, tc.upPath, tc.hadSlash)
		}
	}
}

func TestHandlerUpstreamRedirects(t *testing.T) {
	h := testHandler(t)

	req := httptest.NewRequest("GET", "/upstream", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("/upstream: status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	req = httptest.NewRequest("GET", "/upstream/agents-a1", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/upstream/agents-a1/" {
		t.Fatalf("/upstream/agents-a1: status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	req = httptest.NewRequest("GET", "/upstream/not-a-model/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unknown local model") {
		t.Fatalf("/upstream/not-a-model/: status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest("GET", "/upstream/coding-pool/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/upstream/coding-pool/: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
