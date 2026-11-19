package router

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

const realStream = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"

// progressRouter fronts a backend whose /health answers 503 with progress
// n/10 (capped at 0.2) for the first okAfter polls (never, when okAfter < 0), then 200.
// Other paths return a canned SSE stream.
func progressRouter(t *testing.T, flag bool, okAfter int) http.Handler {
	t.Helper()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/health" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, realStream)
			return
		}
		n := int(polls.Add(1)) - 1
		if okAfter >= 0 && n >= okAfter {
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{"progress": 0.%d}`, min(n+1, 2)) // moves twice, then stalls
	}))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stream_load_progress: %v
stanzas:
  - model_id: a
    command: "fake --port {port}"
    vram_mb: 6000
    spin_up_seconds: 1
    port: %d
`, flag, port)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() { _ = r.Unload("a") })
	old := loadProgressInterval
	loadProgressInterval = 100 * time.Millisecond
	t.Cleanup(func() { loadProgressInterval = old })
	return NewHandler(r, NewLogger(io.Discard, false))
}

func postChat(h http.Handler, stream bool) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"model":"a","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLoadProgressFlagOffHoldsSilently(t *testing.T) {
	rec := postChat(progressRouter(t, false, 2), true)
	if rec.Code != http.StatusOK || rec.Body.String() != realStream {
		t.Fatalf("status %d, body %q: want only the real stream", rec.Code, rec.Body.String())
	}
}

func TestLoadProgressStreamsUpdatesThenRealTokens(t *testing.T) {
	rec := postChat(progressRouter(t, true, 2), true)
	got := rec.Body.String()
	if !strings.HasSuffix(got, realStream) {
		t.Fatalf("real stream must follow the loading chunks, got %q", got)
	}
	loading := strings.TrimSuffix(got, realStream)
	if n := strings.Count(loading, "reasoning_content"); n < 2 {
		t.Fatalf("want progress updates as it changes, got %d in %q", n, loading)
	}
	if !strings.Contains(loading, "10%") || !strings.Contains(loading, "20%") {
		t.Fatalf("updates should carry the health progress, got %q", loading)
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestLoadProgressSkipsNonStreaming(t *testing.T) {
	rec := postChat(progressRouter(t, true, 2), false)
	if rec.Code != http.StatusOK || rec.Body.String() != realStream {
		t.Fatalf("status %d, body %q: non-streaming must only get the real response", rec.Code, rec.Body.String())
	}
}

func TestLoadProgressFailedStartEndsWithError(t *testing.T) {
	rec := postChat(progressRouter(t, true, -1), true)
	got := rec.Body.String()
	if !strings.Contains(got, `"type":"load_failed"`) || !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Fatalf("want an error event then [DONE], got %q", got)
	}
}
