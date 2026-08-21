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

func TestParseTokenStatsJSON(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":26,"completion_tokens":128,"prompt_tokens_details":{"cached_tokens":10}},"timings":{"prompt_n":26,"predicted_n":128,"prompt_per_second":410.5,"predicted_per_second":123.4}}`)
	s := parseTokenStats(body, "application/json")
	if s.PromptTokens != 26 || s.CompletionTokens != 128 || s.CachedTokens != 10 {
		t.Fatalf("tokens = %+v", s)
	}
	if s.PromptPerSecond != 410.5 || s.TokensPerSecond != 123.4 {
		t.Fatalf("rates = %+v", s)
	}
}

func TestParseTokenStatsSSE(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"timings\":{\"prompt_n\":12,\"predicted_n\":8,\"predicted_per_second\":99.5,\"cache_n\":3},\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":8}}\n\n" +
		"data: [DONE]\n")
	s := parseTokenStats(body, "text/event-stream")
	if s.PromptTokens != 12 || s.CompletionTokens != 8 || s.CachedTokens != 3 {
		t.Fatalf("tokens = %+v", s)
	}
	if s.TokensPerSecond != 99.5 {
		t.Fatalf("tok/s = %v", s.TokensPerSecond)
	}
}

func TestCopyHeaderMapRedactsSecrets(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Set("X-Api-Key", "k")
	h.Set("Content-Type", "application/json")
	out := copyHeaderMap(h)
	if out["Authorization"][0] != "[redacted]" || out["X-Api-Key"][0] != "[redacted]" {
		t.Fatalf("secrets not redacted: %+v", out)
	}
	if out["Content-Type"][0] != "application/json" {
		t.Fatalf("content-type = %v", out["Content-Type"])
	}
}

func TestActivityRecordsProxiedRequest(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"loaded"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","usage":{"prompt_tokens":4,"completion_tokens":7},"timings":{"predicted_per_second":42.0}}`))
	}))
	defer peer.Close()

	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.Peer("digger")
	if p == nil {
		t.Fatal("peer digger missing")
	}
	p.BaseURL = peer.URL

	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	h := NewHandler(r, logger)

	body := `{"model":"digger/remote-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	listReq := httptest.NewRequest("GET", "/_router/activity", nil)
	listRec := httptest.NewRecorder()
	h.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("activity list status=%d", listRec.Code)
	}
	var payload struct {
		Entries []ActivityEntry `json:"entries"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if len(payload.Entries) != 1 {
		t.Fatalf("entries = %d, want 1: %+v", len(payload.Entries), payload.Entries)
	}
	e := payload.Entries[0]
	if e.Model != "remote-model" || e.Path != "/v1/chat/completions" || e.Status != 200 {
		t.Fatalf("entry = %+v", e)
	}
	if e.Tokens.PromptTokens != 4 || e.Tokens.CompletionTokens != 7 || e.Tokens.TokensPerSecond != 42 {
		t.Fatalf("tokens = %+v", e.Tokens)
	}
	if !e.HasCapture {
		t.Fatal("expected capture")
	}

	capReq := httptest.NewRequest("GET", "/_router/activity/1", nil)
	capRec := httptest.NewRecorder()
	h.ServeHTTP(capRec, capReq)
	if capRec.Code != http.StatusOK {
		t.Fatalf("capture status=%d body=%s", capRec.Code, capRec.Body.String())
	}
	var cap ActivityCapture
	if err := json.Unmarshal(capRec.Body.Bytes(), &cap); err != nil {
		t.Fatalf("capture json: %v", err)
	}
	if !strings.Contains(cap.ReqBody, `"hi"`) {
		t.Fatalf("req body missing prompt: %s", cap.ReqBody)
	}
	if !strings.Contains(cap.RespBody, `"completion_tokens":7`) {
		t.Fatalf("resp body missing usage: %s", cap.RespBody)
	}
	if got := cap.ReqHeaders["Authorization"]; len(got) != 1 || got[0] != "[redacted]" {
		t.Fatalf("authorization = %v", cap.ReqHeaders["Authorization"])
	}
}

func TestActivityRecordsSSETimings(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: {\"timings\":{\"prompt_n\":9,\"predicted_n\":3,\"predicted_per_second\":55.5,\"prompt_per_second\":400}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer peer.Close()

	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.Peer("digger")
	if p == nil {
		t.Fatal("peer digger missing")
	}
	p.BaseURL = peer.URL

	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	h := NewHandler(r, logger)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"digger/remote-model","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	entries := r.Activity().List()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	tok := entries[0].Tokens
	if tok.PromptTokens != 9 || tok.CompletionTokens != 3 || tok.TokensPerSecond != 55.5 {
		t.Fatalf("sse tokens = %+v", tok)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
}
