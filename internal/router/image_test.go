package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-router/internal/config"
)

// imageTestRouter starts backend and routes a bare sd-server stanza at it: no
// shim, sd-server listening directly, health checked on /v1/models.
func imageTestRouter(t *testing.T, backend http.Handler) *Router {
	t.Helper()
	srv := httptest.NewServer(backend)
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: sd-bare
    command: "sd-server --listen-ip 127.0.0.1 --listen-port {port}"
    vram_mb: 0
    api_type: image
    health_check: /v1/models
    image_pipeline: sd-server
    match: {path_prefix: /sdapi/v1, path_default: true}
    spin_up_seconds: 5
    port: %d
`, port)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "node-a")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() { _ = r.Unload("sd-bare") })
	return r
}

func imageBackend(t *testing.T, txt2img http.HandlerFunc) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[]}`)
	})
	mux.HandleFunc("/sdapi/v1/txt2img", txt2img)
	return mux
}

func TestImagePipelineBareSdServer(t *testing.T) {
	var gotBody []byte
	var gotPath string
	r := imageTestRouter(t, imageBackend(t, func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotBody, _ = io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":["iVBORw0KGgo="]}`)
	}))

	if _, err := r.Load("sd-bare"); err != nil {
		t.Fatalf("Load with health_check /v1/models: %v", err)
	}

	body := `{"prompt":"cat"}`
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/sdapi/v1/txt2img" {
		t.Fatalf("backend path = %q", gotPath)
	}
	if string(gotBody) != body {
		t.Fatalf("backend body = %q, want the client's body unchanged", gotBody)
	}
	if !strings.Contains(rec.Body.String(), `"images"`) {
		t.Fatalf("response = %s", rec.Body.String())
	}
}

func TestImagePipelinePeerRequestAndNotes(t *testing.T) {
	var seenPeer, gotPrompt string
	r := imageTestRouter(t, imageBackend(t, func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		gotPrompt = in.Prompt
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":["ok"]}`)
	}))
	r.imageSteps = func(*config.Stanza) []imageStep {
		return []imageStep{{
			Rewrite: func(req *http.Request, notes *imageNotes) error {
				seenPeer = req.Header.Get("X-Model-Router-Peer")
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return err
				}
				body["prompt"] = "rewritten"
				out, _ := json.Marshal(body)
				req.Body = io.NopCloser(bytes.NewReader(out))
				req.ContentLength = int64(len(out))
				notes.Add("prompt rewritten")
				return nil
			},
		}}
	}

	body := `{"prompt":"cat"}`
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Model-Router-Peer", "node-b")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if seenPeer != "node-b" {
		t.Fatalf("step saw peer %q, want node-b", seenPeer)
	}
	if gotPrompt != "rewritten" {
		t.Fatalf("backend prompt = %q, want the step's rewrite", gotPrompt)
	}

	entries := r.Activity().List()
	if len(entries) == 0 {
		t.Fatal("no activity entry")
	}
	cap, ok := r.Activity().Capture(entries[len(entries)-1].ID)
	if !ok {
		t.Fatal("no activity capture")
	}
	if !strings.Contains(cap.ReqBody, `"prompt":"cat"`) {
		t.Fatalf("capture body = %q, want the client's original", cap.ReqBody)
	}
	if len(cap.Notes) != 1 || cap.Notes[0] != "prompt rewritten" {
		t.Fatalf("capture notes = %v", cap.Notes)
	}
}

func TestImagePipelineResponseStepRetriesOnce(t *testing.T) {
	attempts := 0
	r := imageTestRouter(t, imageBackend(t, func(w http.ResponseWriter, req *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts == 1 {
			fmt.Fprint(w, `{"blocked":true}`)
			return
		}
		fmt.Fprint(w, `{"images":["ok"]}`)
	}))
	r.imageSteps = func(*config.Stanza) []imageStep {
		return []imageStep{{
			After: func(req *http.Request, status int, h http.Header, body []byte, notes *imageNotes) bool {
				h.Set("X-Image-Pipeline", "checked")
				return bytes.Contains(body, []byte(`"blocked":true`))
			},
		}}
	}

	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(`{"prompt":"cat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)

	if attempts != 2 {
		t.Fatalf("backend attempts = %d, want one retry", attempts)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"images"`) {
		t.Fatalf("response = %d %s, want the retried body", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Image-Pipeline"); got != "checked" {
		t.Fatalf("response header = %q, want checked", got)
	}
}
