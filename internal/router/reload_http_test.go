package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"model-router/internal/config"
)

// reloadFixture is a router with a reloader on a temp config file, behind the access gate.
func reloadFixture(t *testing.T) (*Router, string, http.Handler) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router.yaml")
	if err := os.WriteFile(path, []byte("stanzas: []\nlisten: 127.0.0.1:19001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	log := NewLogger(io.Discard, false)
	r := New(cfg, log, "node-a")
	r.SetReloader(config.NewReloader(path, cfg, r.Apply, log))
	h := NewAccessHandler(config.Security{APIKeys: []string{testKey}}, NewHandler(r, log))
	return r, path, h
}

func postReload(h http.Handler, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/_router/reload", nil)
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func reloadPayload(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return out
}

// statusReload is the "reload" object the UI reads from /_router/status.
func statusReload(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/_router/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var st struct {
		Reload map[string]any `json:"reload"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st.Reload
}

func TestReloadEndpointValidAppliesAndReportsTime(t *testing.T) {
	r, path, h := reloadFixture(t)
	if got := statusReload(t, h); got != nil {
		t.Fatalf("reload reported before any ran: %v", got)
	}
	if err := os.WriteFile(path, []byte("stanzas: []\nlisten: 127.0.0.1:19001\nstart_port: 25000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := r.Config()
	rec := postReload(h, "127.0.0.1:1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := reloadPayload(t, rec)
	if out["status"] != "reloaded" || out["ok"] != true || out["at"] == "" {
		t.Fatalf("payload = %v", out)
	}
	if r.Config() == old || r.Config().StartPort != 25000 {
		t.Error("new config not applied")
	}
	if st := statusReload(t, h); st["ok"] != true || st["at"] != out["at"] {
		t.Errorf("status reload = %v, want the POST result", st)
	}
}

func TestReloadEndpointInvalidKeepsRunningConfig(t *testing.T) {
	r, path, h := reloadFixture(t)
	old := r.Config()
	if err := os.WriteFile(path, []byte("stanzas: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := postReload(h, "127.0.0.1:1", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := reloadPayload(t, rec)
	if out["status"] != "rejected" || out["ok"] != false || out["error"] == "" {
		t.Fatalf("payload = %v", out)
	}
	if r.Config() != old {
		t.Error("running config changed after a rejected reload")
	}
	if st := statusReload(t, h); st["ok"] != false || st["error"] == "" {
		t.Errorf("status reload = %v, want the error", st)
	}
}

func TestReloadEndpointAuth(t *testing.T) {
	_, _, h := reloadFixture(t)
	if rec := postReload(h, "203.0.113.9:1", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("public without key = %d, want 401", rec.Code)
	}
	if rec := postReload(h, "203.0.113.9:1", map[string]string{"Authorization": "Bearer " + testKey}); rec.Code != http.StatusOK {
		t.Errorf("public with key = %d, want 200", rec.Code)
	}
	for _, remote := range []string{"127.0.0.1:1", "192.168.1.5:1", "100.64.0.1:1"} {
		if rec := postReload(h, remote, nil); rec.Code != http.StatusOK {
			t.Errorf("%s without key = %d, want 200", remote, rec.Code)
		}
	}
}

func TestReloadEndpointMethod(t *testing.T) {
	_, _, h := reloadFixture(t)
	req := httptest.NewRequest("GET", "/_router/reload", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", rec.Code)
	}
}
