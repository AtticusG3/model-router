package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-router/internal/config"
)

func TestHTTPServerPeerRouting(t *testing.T) {
	var loadPath, proxyPath string
	var loadBody, proxyBody []byte
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if req.URL.Path == "/_router/load" {
			loadPath = req.URL.Path
			loadBody = body
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"loaded"}`))
			return
		}
		proxyPath = req.URL.Path
		proxyBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-peer","object":"chat.completion"}`))
	}))
	defer peer.Close()

	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.Peer("digger")
	if p == nil {
		t.Fatal("peer digger missing from smoke config")
	}
	p.BaseURL = peer.URL

	r := New(cfg, NewLogger(io.Discard, false), "router-a")
	h := NewHandler(r, NewLogger(io.Discard, false))

	body := `{"model":"digger/remote-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if loadPath != "/_router/load" {
		t.Fatalf("peer load path = %q, want /_router/load", loadPath)
	}
	var load struct {
		ModelID string `json:"model_id"`
	}
	if err := json.Unmarshal(loadBody, &load); err != nil {
		t.Fatalf("load body: %v", err)
	}
	if load.ModelID != "remote-model" {
		t.Fatalf("load model_id = %q, want remote-model", load.ModelID)
	}
	if proxyPath != "/v1/chat/completions" {
		t.Fatalf("proxied path = %q, want /v1/chat/completions", proxyPath)
	}
	var proxied struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(proxyBody, &proxied); err != nil {
		t.Fatalf("proxied body: %v", err)
	}
	if proxied.Model != "remote-model" {
		t.Fatalf("rewritten model = %q, want remote-model", proxied.Model)
	}
}

func TestHTTPServerWebUI(t *testing.T) {
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	h := NewHandler(r, logger)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("root redirect status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	req = httptest.NewRequest("GET", "/ui/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("UI status = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(body, "model-router") {
		t.Fatalf("UI response missing HTML shell: content-type=%q", rec.Header().Get("Content-Type"))
	}
	for _, want := range []string{`id="chat-model"`, `id="image-model"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("UI shell missing %q", want)
		}
	}
	if strings.Contains(body, "juggernaut") {
		t.Fatal("image options still use id/name regex")
	}

	req = httptest.NewRequest("GET", "/_router/status", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status endpoint = %d, body = %s", rec.Code, rec.Body.String())
	}
	var snapshot struct {
		Models []struct {
			ID      string `json:"id"`
			APIType string `json:"api_type"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("status json: %v", err)
	}
	var imageType string
	for _, m := range snapshot.Models {
		if m.ID == "krea-2-turbo" {
			imageType = m.APIType
			break
		}
	}
	if imageType != "image" {
		t.Fatalf("krea-2-turbo api_type = %q, want image (status must expose api_type for the UI dropdown)", imageType)
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
