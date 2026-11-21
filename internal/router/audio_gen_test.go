package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"model-router/internal/config"
)

// audioJobRouter serves one api_type audio_gen stanza, music, from the fake
// job backend, so a model-less request has exactly one place to go.
func audioJobRouter(t *testing.T, b *jobBackend) (*Router, http.Handler) {
	t.Helper()
	u, err := url.Parse(b.srv.URL)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
stanzas:
  - model_id: music
    command: "fake --port {port}"
    vram_mb: 6000
    api_type: audio_gen
    health_check: /health
    spin_up_seconds: 5
    idle_ttl_seconds: 1
    port: %s
`, u.Port())))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() {
		r.Shutdown()
		_ = r.Unload("music")
	})
	return r, NewHandler(r, NewLogger(io.Discard, false))
}

func submitAudioJob(t *testing.T, h http.Handler, body string) string {
	t.Helper()
	rec := doJob(h, http.MethodPost, "/v1/audio/generations", body)
	var v struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.ID == "" || v.Object != "audio.generation" {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	return v.ID
}

// A model-less audio job reaches the only audio_gen stanza, holds it while it
// runs, polls through to completed, serves its audio, logs one Activity row
// under its own path, and is unknown under /v1/videos.
func TestAudioGenJobLifecycle(t *testing.T) {
	fastJobPolls(t)
	b := newJobBackend(t, 400*time.Millisecond)
	r, h := audioJobRouter(t, b)

	id := submitAudioJob(t, h, `{"prompt":"a slow piano waltz","lyrics":"[Instrumental]","seconds":30,"seed":7}`)
	if got := r.occupancyOf("music"); got != 1 {
		t.Fatalf("occupancy after submit = %d, want 1 (the job hold)", got)
	}
	if rec := doJob(h, http.MethodGet, "/v1/videos/"+id, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("audio id under /v1/videos = %d, want 404", rec.Code)
	}
	waitJob(t, "completed", func() bool {
		rec := doJob(h, http.MethodGet, "/v1/audio/generations/"+id, "")
		var v struct {
			Status string `json:"status"`
		}
		return rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &v) == nil && v.Status == "completed"
	})
	waitJob(t, "hold released", func() bool { return r.occupancyOf("music") == 0 })
	rec := doJob(h, http.MethodGet, "/v1/audio/generations/"+id+"/content", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/mpeg" || rec.Body.String() != "fake-mp3-bytes" {
		t.Fatalf("content = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if row, ok := jobRow(r, id); !ok || row.JobState != "completed" || row.Path != "/v1/audio/generations" {
		t.Fatalf("activity row = %+v (found %v), want completed on /v1/audio/generations", row, ok)
	}
}

// DELETE cancels a running audio job on the backend, frees its hold and
// forgets the id.
func TestAudioGenJobDelete(t *testing.T) {
	fastJobPolls(t)
	b := newJobBackend(t, time.Minute)
	r, h := audioJobRouter(t, b)

	id := submitAudioJob(t, h, `{"prompt":"rain on a tin roof"}`)
	if rec := doJob(h, http.MethodDelete, "/v1/audio/generations/"+id, ""); rec.Code != http.StatusOK || b.deletes.Load() != 1 {
		t.Fatalf("delete: %d %s (backend deletes %d)", rec.Code, rec.Body.String(), b.deletes.Load())
	}
	if got := r.occupancyOf("music"); got != 0 {
		t.Fatalf("occupancy after delete = %d, want 0", got)
	}
	if rec := doJob(h, http.MethodGet, "/v1/audio/generations/"+id, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("poll after delete = %d, want 404", rec.Code)
	}
}

// A multipart submit with a reference clip is recorded as a job like JSON.
func TestAudioGenMultipartSubmit(t *testing.T) {
	fastJobPolls(t)
	b := newJobBackend(t, time.Minute)
	r, h := audioJobRouter(t, b)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("prompt", "the same tune on strings")
	fw, _ := mw.CreateFormFile("reference_audio", "hum.wav")
	_, _ = fw.Write(bytes.Repeat([]byte{1}, 256<<10))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/generations", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || r.RunningJobs()["music"] != 1 {
		t.Fatalf("multipart submit = %d %s, running %v", rec.Code, rec.Body.String(), r.RunningJobs())
	}
}

// A 30 MB result reaches the client while the backend is still writing it:
// the router streams the content, it does not buffer it.
func TestAudioGenContentStreamsLargeResult(t *testing.T) {
	fastJobPolls(t)
	b := newJobBackend(t, 50*time.Millisecond)
	const total = 30 << 20
	firstSeen := make(chan struct{})
	b.content = func(w http.ResponseWriter) {
		w.Header().Set("Content-Length", fmt.Sprint(total))
		chunk := make([]byte, 1<<20)
		_, _ = w.Write(chunk)
		_ = http.NewResponseController(w).Flush()
		select { // hold the rest back until the client has the first chunk
		case <-firstSeen:
		case <-time.After(5 * time.Second):
			return
		}
		for range total/len(chunk) - 1 {
			_, _ = w.Write(chunk)
		}
	}
	_, h := audioJobRouter(t, b)
	srv := httptest.NewServer(h)
	defer srv.Close()

	id := submitAudioJob(t, h, `{"prompt":"a long ambient drone"}`)
	waitJob(t, "completed", func() bool { return b.status(id) == "completed" })
	resp, err := http.Get(srv.URL + "/v1/audio/generations/" + id + "/content")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil {
		t.Fatalf("first byte: %v", err)
	}
	close(firstSeen)
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || n+1 != total {
		t.Fatalf("read %d bytes (%v), want %d", n+1, err, total)
	}
}
