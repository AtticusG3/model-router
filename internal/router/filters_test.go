package router

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-router/internal/config"
)

// filterRouter runs a stanza with the given filters YAML against a fake
// backend. send posts a body and returns the body the backend received.
func filterRouter(t *testing.T, filters string) (r *Router, send func(path, ct, body string) string) {
	t.Helper()
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		got = string(b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(backend.Close)
	port := backend.Listener.Addr().(*net.TCPAddr).Port
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
stanzas:
  - model_id: chat-a
    command: "fake --port {port}"
    vram_mb: 0
    health_check: /
    spin_up_seconds: 5
    port: %d
    filters:
%s
`, port, filters)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r = New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() { _ = r.Unload("chat-a") })
	h := NewHandler(r, NewLogger(io.Discard, false))
	return r, func(path, ct, body string) string {
		req := httptest.NewRequest("POST", path, bytes.NewBufferString(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		return got
	}
}

func TestFiltersStripAndSet(t *testing.T) {
	_, send := filterRouter(t, `
      stripParams: [temperature]
      setParams:
        top_p: 0.5
        seed?: 7
        top_k?: 40`)
	got := send("/v1/chat/completions", "application/json",
		`{"model":"chat-a","temperature":1.5,"top_p":0.9,"top_k":10,"big":12345678901234567890}`)
	if strings.Contains(got, "temperature") {
		t.Errorf("temperature not stripped: %s", got)
	}
	for _, want := range []string{`"top_p":0.5`, `"top_k":10`, `"seed":7`, `"model":"chat-a"`, `"big":12345678901234567890`} {
		if !strings.Contains(got, want) {
			t.Errorf("body %s missing %s", got, want)
		}
	}
}

func TestFiltersLeaveNonJSONAlone(t *testing.T) {
	_, send := filterRouter(t, `
      stripParams: [temperature]
      setParams: {top_p: 0.5}`)
	form := "model=chat-a&temperature=1"
	if got := send("/v1/audio/transcriptions", "application/x-www-form-urlencoded", form); got != form {
		t.Errorf("form body changed: %q", got)
	}
}

func TestApplyFiltersIgnoresNonObjectJSON(t *testing.T) {
	for _, body := range []string{`[1,2]`, `null`, `not json`} {
		req := httptest.NewRequest("POST", "/x", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		if err := applyFilters(req, []string{"a"}, map[string]any{"b": 1}); err != nil {
			t.Fatal(err)
		}
		if got, _ := io.ReadAll(req.Body); string(got) != body {
			t.Errorf("body %q became %q", body, got)
		}
	}
}

func TestFiltersAliasRoutesToSameStanza(t *testing.T) {
	r, send := filterRouter(t, `
      setParams: {temperature: 0.7}
      setParamsByID:
        "${MODEL_ID}:high":
          temperature: 0.1
          reasoning_effort: high`)
	const p = "/v1/chat/completions"
	got := send(p, "application/json", `{"model":"chat-a:high"}`)
	if !strings.Contains(got, `"temperature":0.1`) || !strings.Contains(got, `"reasoning_effort":"high"`) {
		t.Errorf("alias params not applied: %s", got)
	}
	got = send(p, "application/json", `{"model":"chat-a"}`)
	if !strings.Contains(got, `"temperature":0.7`) || strings.Contains(got, "reasoning_effort") {
		t.Errorf("plain id picked up alias params: %s", got)
	}
	var ids []string
	for _, ms := range r.AllModelStatuses() {
		ids = append(ids, ms.ID)
	}
	if strings.Join(ids, ",") != "chat-a,chat-a:high" {
		t.Errorf("catalog ids = %v, want chat-a and chat-a:high", ids)
	}
}

func TestFiltersPeer(t *testing.T) {
	var got string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if req.URL.Path != "/_router/load" {
			got = string(b)
		}
		_, _ = w.Write([]byte(`{"status":"loaded"}`))
	}))
	defer peer.Close()
	cfg, err := config.Parse([]byte(`
peers:
  - name: far
    base_url: ` + peer.URL + `
    models: [remote-model]
    filters:
      stripParams: [temperature]
`))
	if err != nil {
		t.Fatal(err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"far/remote-model","temperature":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(got, "temperature") || !strings.Contains(got, `"model":"remote-model"`) {
		t.Errorf("status=%d peer body=%s", rec.Code, got)
	}
}
