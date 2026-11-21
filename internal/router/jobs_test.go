package router

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

// jobBackend is a fake video backend with the /v1/videos job API. A job is
// queued for the first third of d, in_progress until d, then completed. Ids are
// random, as on real backends. submitDelay slows the submit answer.
type jobBackend struct {
	srv         *httptest.Server
	d           time.Duration
	submitDelay time.Duration
	mu          sync.Mutex
	jobs        map[string]time.Time
	polls       atomic.Int32
	deletes     atomic.Int32
}

func newJobBackend(t *testing.T, d time.Duration) *jobBackend {
	t.Helper()
	b := &jobBackend{d: d, jobs: map[string]time.Time{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {})
	mux.HandleFunc("POST /v1/videos", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(b.submitDelay)
		id := "video_" + rand.Text()
		b.mu.Lock()
		b.jobs[id] = time.Now()
		b.mu.Unlock()
		writeJSON(w, map[string]string{"id": id, "object": "video", "status": "queued"})
	})
	mux.HandleFunc("POST /v1/videos/{id}/remix", func(w http.ResponseWriter, req *http.Request) {
		if b.status(req.PathValue("id")) == "" {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		id := "video_" + rand.Text()
		b.mu.Lock()
		b.jobs[id] = time.Now()
		b.mu.Unlock()
		writeJSON(w, map[string]string{"id": id, "object": "video", "status": "queued", "remixed_from_video_id": req.PathValue("id")})
	})
	mux.HandleFunc("GET /v1/videos/{id}", func(w http.ResponseWriter, req *http.Request) {
		b.polls.Add(1)
		st := b.status(req.PathValue("id"))
		if st == "" {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]string{"id": req.PathValue("id"), "status": st})
	})
	mux.HandleFunc("GET /v1/videos/{id}/content", func(w http.ResponseWriter, req *http.Request) {
		if b.status(req.PathValue("id")) != "completed" {
			http.Error(w, "not ready", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("fake-mp4-bytes"))
	})
	mux.HandleFunc("DELETE /v1/videos/{id}", func(w http.ResponseWriter, req *http.Request) {
		b.deletes.Add(1)
		b.mu.Lock()
		delete(b.jobs, req.PathValue("id"))
		b.mu.Unlock()
		writeJSON(w, map[string]any{"id": req.PathValue("id"), "deleted": true})
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func (b *jobBackend) status(id string) string {
	b.mu.Lock()
	created, ok := b.jobs[id]
	b.mu.Unlock()
	switch age := time.Since(created); {
	case !ok:
		return ""
	case age < b.d/3:
		return "queued"
	case age < b.d:
		return "in_progress"
	default:
		return "completed"
	}
}

// jobRouter serves stanza vid (api_type video) from the fake backend, and
// stanza other, which needs the same card's VRAM, through fake processes.
func jobRouter(t *testing.T, b *jobBackend, node, extra string) (*Router, http.Handler) {
	t.Helper()
	u, err := url.Parse(b.srv.URL)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
%s
stanzas:
  - model_id: vid
    command: "fake --port {port}"
    vram_mb: 6000
    api_type: video
    health_check: /health
    spin_up_seconds: 5
    idle_ttl_seconds: 1
    port: %s
  - model_id: other
    command: "fake --port {port}"
    vram_mb: 6000
    health_check: /health
    port: 5999
`, extra, u.Port())))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), node)
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() {
		r.Shutdown()
		_ = r.Unload("vid")
	})
	return r, NewHandler(r, NewLogger(io.Discard, false))
}

func fastJobPolls(t *testing.T) {
	t.Helper()
	old := jobPollInterval
	jobPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { jobPollInterval = old })
}

func doJob(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func submitJob(t *testing.T, h http.Handler, model string) string {
	t.Helper()
	rec := doJob(h, http.MethodPost, "/v1/videos", `{"model":"`+model+`","prompt":"a lighthouse at dusk"}`)
	var v struct {
		ID string `json:"id"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.ID == "" {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	return v.ID
}

func jobStatus(t *testing.T, h http.Handler, id string) string {
	t.Helper()
	rec := doJob(h, http.MethodGet, "/v1/videos/"+id, "")
	var v struct {
		Status string `json:"status"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
		t.Fatalf("poll %s: %d %s", id, rec.Code, rec.Body.String())
	}
	return v.Status
}

func waitJob(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func jobRow(r *Router, id string) (ActivityEntry, bool) {
	for _, e := range r.Activity().List() {
		if e.JobID == id {
			return e, true
		}
	}
	return ActivityEntry{}, false
}

// A job is polled through to completed, holds its model against the idle
// sweep and eviction while it runs, shows in status and metrics, and its
// content streams once done; then the idle TTL applies again.
func TestJobLifecycleHoldsModelUntilDone(t *testing.T) {
	fastJobPolls(t)
	b := newJobBackend(t, 600*time.Millisecond)
	r, h := jobRouter(t, b, "test", "")

	id := submitJob(t, h, "vid")
	if got := r.occupancyOf("vid"); got != 1 {
		t.Fatalf("occupancy after submit = %d, want 1 (the job hold)", got)
	}
	if !strings.Contains(doJob(h, http.MethodGet, "/metrics", "").Body.String(), `model_router_jobs_running{model="vid"} 1`) {
		t.Fatal("/metrics should report one running job for vid")
	}
	var st struct {
		Jobs map[string]int `json:"jobs"`
	}
	if err := json.Unmarshal(doJob(h, http.MethodGet, "/_router/status", "").Body.Bytes(), &st); err != nil || st.Jobs["vid"] != 1 {
		t.Fatalf("/_router/status jobs = %v (%v), want vid: 1", st.Jobs, err)
	}

	// Held: the idle sweep and a competing load leave it alone.
	r.managed["vid"].setLastUsed(time.Now().Add(-time.Hour))
	r.SweepStale()
	if _, err := r.Load("other"); err == nil {
		t.Fatal("other must not load by evicting a model with a running job")
	}
	if r.managed["vid"] == nil {
		t.Fatal("a model with a running job was unloaded")
	}

	seen := map[string]bool{}
	waitJob(t, "completed", func() bool {
		s := jobStatus(t, h, id)
		seen[s] = true
		return s == "completed"
	})
	if !seen["queued"] || !seen["in_progress"] {
		t.Fatalf("statuses seen = %v, want queued and in_progress before completed", seen)
	}
	waitJob(t, "hold released", func() bool { return r.occupancyOf("vid") == 0 })

	rec := doJob(h, http.MethodGet, "/v1/videos/"+id+"/content", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "video/mp4" || rec.Body.String() != "fake-mp4-bytes" {
		t.Fatalf("content = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	row, ok := jobRow(r, id)
	if !ok || row.JobState != "completed" || row.Path != "/v1/videos" || row.DurationMs < 500 {
		t.Fatalf("activity row = %+v (found %v), want one completed row spanning the job", row, ok)
	}

	r.managed["vid"].setLastUsed(time.Now().Add(-time.Hour))
	r.SweepStale()
	if r.managed["vid"] != nil {
		t.Fatal("after the job ends the idle TTL must apply again")
	}
}

// A client that drops while its submit is in flight still leaves a job the
// router holds and watches to the end.
func TestJobSurvivesSubmittingClientDisconnect(t *testing.T) {
	fastJobPolls(t)
	b := newJobBackend(t, 300*time.Millisecond)
	b.submitDelay = 200 * time.Millisecond
	r, h := jobRouter(t, b, "test", "")
	if _, err := r.Load("vid"); err != nil {
		t.Fatalf("load: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/videos", strings.NewReader(`{"model":"vid"}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("the client should have given up before the submit answered")
	}

	waitJob(t, "job recorded", func() bool { return r.RunningJobs()["vid"] == 1 })
	waitJob(t, "job completed", func() bool { return r.RunningJobs()["vid"] == 0 })
	var row ActivityEntry
	for _, e := range r.Activity().List() {
		if e.JobID != "" {
			row = e
		}
	}
	if row.JobState != "completed" {
		t.Fatalf("job row = %+v, want completed", row)
	}
}

// A job spilled to a peer router is recorded against that peer, and its
// follow-ups and the router's own polls go there.
func TestJobOnPeerIsPolledOnThatPeer(t *testing.T) {
	fastJobPolls(t)
	b := newJobBackend(t, 300*time.Millisecond)
	peer, peerH := jobRouter(t, b, "alpha", "")
	peerSrv := httptest.NewServer(peerH)
	defer peerSrv.Close()

	origin := spilloverRouter(t, "start_port: 5900\npeers:\n  - name: alpha\n    kind: router\n    base_url: "+peerSrv.URL+"\n    models: [vid]\n")
	origin.peers.Set("alpha", &Telemetry{Node: "alpha"})
	t.Cleanup(origin.Shutdown)
	h := NewHandler(origin, NewLogger(io.Discard, false))

	id := submitJob(t, h, "vid")
	if origin.RunningJobs()["vid"] != 1 || peer.RunningJobs()["vid"] != 1 {
		t.Fatalf("running jobs: origin %v, peer %v; want vid on both", origin.RunningJobs(), peer.RunningJobs())
	}
	// The peer's submit slot may still be on its way out; the job hold stays.
	waitJob(t, "peer holds the job's slot", func() bool { return peer.occupancyOf("vid") == 1 })
	waitJob(t, "completed via peer", func() bool { return jobStatus(t, h, id) == "completed" })
	waitJob(t, "both holds released", func() bool {
		return origin.RunningJobs()["vid"] == 0 && peer.RunningJobs()["vid"] == 0
	})
	if b.polls.Load() == 0 {
		t.Fatal("the backend behind the peer was never polled")
	}
	if row, ok := jobRow(origin, id); !ok || row.Target != "alpha" || row.JobState != "completed" {
		t.Fatalf("origin activity row = %+v (found %v), want completed on alpha", row, ok)
	}
}

// DELETE cancels the job on the backend, frees the hold at once and forgets
// the id.
func TestJobDeleteCancelsAndFreesHold(t *testing.T) {
	fastJobPolls(t)
	b := newJobBackend(t, time.Minute)
	r, h := jobRouter(t, b, "test", "")

	id := submitJob(t, h, "vid")
	if rec := doJob(h, http.MethodDelete, "/v1/videos/"+id, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if b.deletes.Load() != 1 {
		t.Fatal("DELETE did not reach the backend")
	}
	if got := r.occupancyOf("vid"); got != 0 {
		t.Fatalf("occupancy after delete = %d, want 0", got)
	}
	if row, ok := jobRow(r, id); !ok || row.JobState != "cancelled" {
		t.Fatalf("activity row = %+v (found %v), want cancelled", row, ok)
	}
	if rec := doJob(h, http.MethodGet, "/v1/videos/"+id, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("poll after delete = %d, want 404", rec.Code)
	}
}

// A remix POST makes a child job that the router records and holds like a
// submit, so the model stays loaded and the child stays reachable.
func TestJobRemixRecordsAndHoldsChild(t *testing.T) {
	fastJobPolls(t)
	b := newJobBackend(t, 400*time.Millisecond)
	r, h := jobRouter(t, b, "test", "")

	parent := submitJob(t, h, "vid")
	rec := doJob(h, http.MethodPost, "/v1/videos/"+parent+"/remix", `{"prompt":"the same lighthouse at dawn"}`)
	var v struct {
		ID string `json:"id"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.ID == "" || v.ID == parent {
		t.Fatalf("remix: %d %s", rec.Code, rec.Body.String())
	}
	if got := r.occupancyOf("vid"); got != 2 {
		t.Fatalf("occupancy after remix = %d, want 2 (parent and child holds)", got)
	}
	if got := r.RunningJobs()["vid"]; got != 2 {
		t.Fatalf("running jobs = %d, want 2", got)
	}
	waitJob(t, "child completed", func() bool { return jobStatus(t, h, v.ID) == "completed" })
	waitJob(t, "holds released", func() bool { return r.occupancyOf("vid") == 0 })
	if row, ok := jobRow(r, v.ID); !ok || row.JobState != "completed" {
		t.Fatalf("child activity row = %+v (found %v), want completed", row, ok)
	}
}

// An id the router does not know is 404 with a resubmit hint, and nothing is
// loaded or proxied for it.
func TestJobUnknownIDIs404(t *testing.T) {
	b := newJobBackend(t, time.Second)
	r, h := jobRouter(t, b, "test", "")
	for _, path := range []string{"/v1/videos/video_nope", "/v1/videos/video_nope/content"} {
		rec := doJob(h, http.MethodGet, path, "")
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "submit it again") {
			t.Fatalf("%s = %d %q, want 404 with a hint", path, rec.Code, rec.Body.String())
		}
	}
	if r.managed["vid"] != nil || b.polls.Load() != 0 {
		t.Fatal("an unknown job id must not load or reach a backend")
	}
}

// A full table drops its oldest finished job and never a running one.
func TestJobTableCapEvictsOldestFinishedOnly(t *testing.T) {
	r := spilloverRouter(t, "start_port: 5900\njob_table_max: 2\n")
	t.Cleanup(r.Shutdown)
	tgt := Target{Peer: "alpha", PeerID: "vid"}
	add := func(id string) *job {
		j := &job{id: id, model: "vid", target: "alpha", tgt: tgt, base: "http://127.0.0.1:9", code: 200, created: time.Now()}
		r.recordJob(j)
		time.Sleep(time.Millisecond) // distinct created times
		return j
	}
	r.endJob(add("old-done"), "completed")
	add("running")
	add("third")  // full: old-done goes
	add("fourth") // full of running jobs: kept over the cap
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jobs["old-done"] != nil {
		t.Fatal("the oldest finished job should have been dropped")
	}
	for _, id := range []string{"running", "third", "fourth"} {
		if r.jobs[id] == nil {
			t.Fatalf("running job %s was dropped", id)
		}
	}
}

// sdcppBackend fakes sd-server's /sdcpp/v1 job API. A job is queued for the
// first third of d, generating until d, then completed with a WebM result.
type sdcppBackend struct {
	srv     *httptest.Server
	d       time.Duration
	mu      sync.Mutex
	jobs    map[string]*sdcppState
	cancels atomic.Int32
	// badResult serves a result whose b64_json is not a string, so only a
	// status poll that skips result can read the record.
	badResult bool
}

type sdcppState struct {
	created   time.Time
	cancelled bool
	initImage string
	seed      string
}

func newSDCppBackend(t *testing.T, d time.Duration) *sdcppBackend {
	t.Helper()
	b := &sdcppBackend{d: d, jobs: map[string]*sdcppState{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {})
	mux.HandleFunc("POST /sdcpp/v1/vid_gen", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			InitImage string      `json:"init_image"`
			Seed      json.Number `json:"seed"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		id := "job_" + rand.Text()
		b.mu.Lock()
		b.jobs[id] = &sdcppState{created: time.Now(), initImage: body.InitImage, seed: body.Seed.String()}
		b.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"id": id, "status": "queued"})
	})
	mux.HandleFunc("GET /sdcpp/v1/jobs/{id}", func(w http.ResponseWriter, req *http.Request) {
		id := req.PathValue("id")
		b.mu.Lock()
		st, ok := b.jobs[id]
		b.mu.Unlock()
		if !ok {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		rec := map[string]any{"id": id, "created": st.created.Unix()}
		switch age := time.Since(st.created); {
		case st.cancelled:
			rec["status"] = "cancelled"
		case age < b.d/3:
			rec["status"] = "queued"
		case age < b.d:
			rec["status"] = "generating"
			rec["progress"] = map[string]int{"completed": 1, "total": 2}
		default:
			rec["status"] = "completed"
			rec["progress"] = map[string]int{"completed": 1, "total": 1}
			if b.badResult {
				rec["result"] = map[string]any{"kind": "vid_gen", "mime_type": "video/webm", "b64_json": 12345}
			} else {
				rec["result"] = map[string]any{
					"kind": "vid_gen", "mime_type": "video/webm",
					"b64_json": base64.StdEncoding.EncodeToString([]byte("fake-webm-bytes")),
				}
			}
		}
		writeJSON(w, rec)
	})
	mux.HandleFunc("POST /sdcpp/v1/jobs/{id}/cancel", func(w http.ResponseWriter, req *http.Request) {
		b.mu.Lock()
		st, ok := b.jobs[req.PathValue("id")]
		if ok {
			st.cancelled = true
		}
		b.mu.Unlock()
		if !ok {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		b.cancels.Add(1)
		writeJSON(w, map[string]any{"id": req.PathValue("id"), "status": "cancelled"})
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

// sdcppJobRouter serves one api_type video stanza through the sdcpp adapter.
func sdcppJobRouter(t *testing.T, b *sdcppBackend) (*Router, http.Handler) {
	t.Helper()
	u, err := url.Parse(b.srv.URL)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
stanzas:
  - model_id: vid
    command: "fake --port {port}"
    vram_mb: 6000
    api_type: video
    capabilities: [audio_out]
    job: {adapter: sdcpp}
    health_check: /health
    spin_up_seconds: 5
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
		_ = r.Unload("vid")
	})
	return r, NewHandler(r, NewLogger(io.Discard, false))
}

// The sdcpp adapter carries a job through submit, poll, content and delete,
// and publishes the stanza's capabilities.
func TestVideoAdapterLifecycle(t *testing.T) {
	fastJobPolls(t)
	b := newSDCppBackend(t, 300*time.Millisecond)
	r, h := sdcppJobRouter(t, b)

	var models struct {
		Data []struct {
			ID           string   `json:"id"`
			Capabilities []string `json:"capabilities"`
		} `json:"data"`
	}
	if rec := doJob(h, http.MethodGet, "/v1/models", ""); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &models) != nil {
		t.Fatalf("/v1/models: %d %s", rec.Code, rec.Body.String())
	}
	if len(models.Data) != 1 || models.Data[0].ID != "vid" || !slices.Contains(models.Data[0].Capabilities, "audio_out") {
		t.Fatalf("/v1/models data = %+v, want vid with audio_out", models.Data)
	}

	id := submitJob(t, h, "vid")
	waitJob(t, "adapter job completed", func() bool { return jobStatus(t, h, id) == "completed" })
	rec := doJob(h, http.MethodGet, "/v1/videos/"+id+"/content", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "video/webm" || rec.Body.String() != "fake-webm-bytes" {
		t.Fatalf("content = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}

	id = submitJob(t, h, "vid")
	if rec := doJob(h, http.MethodDelete, "/v1/videos/"+id, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if b.cancels.Load() != 1 {
		t.Fatal("DELETE did not reach the backend cancel route")
	}
	if got := r.occupancyOf("vid"); got != 0 {
		t.Fatalf("occupancy after delete = %d, want 0", got)
	}
}

// A model-less submit reaches the only video stanza and is translated onto
// sd-server's field names.
func TestVideoAdapterModelLessSubmit(t *testing.T) {
	fastJobPolls(t)
	b := newSDCppBackend(t, 300*time.Millisecond)
	_, h := sdcppJobRouter(t, b)

	rec := doJob(h, http.MethodPost, "/v1/videos", `{"prompt":"a paper kite","size":"256x256","seconds":1}`)
	var v struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.ID == "" || v.Status != "queued" {
		t.Fatalf("model-less submit = %d %s", rec.Code, rec.Body.String())
	}
	waitJob(t, "model-less job completed", func() bool { return jobStatus(t, h, v.ID) == "completed" })
}

// A 20 MB multipart input_reference survives the adapter translation intact.
func TestVideoAdapterMultipartReferenceArrivesIntact(t *testing.T) {
	fastJobPolls(t)
	b := newSDCppBackend(t, time.Minute)
	_, h := sdcppJobRouter(t, b)

	reference := bytes.Repeat([]byte{0x5a}, 20<<20)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("prompt", "animate this frame")
	_ = mw.WriteField("seconds", "1")
	fw, err := mw.CreateFormFile("input_reference", "frame.png")
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	_, _ = fw.Write(reference)
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/videos", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var v struct {
		ID string `json:"id"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.ID == "" {
		t.Fatalf("multipart submit = %d %s", rec.Code, rec.Body.String())
	}

	b.mu.Lock()
	st := b.jobs[v.ID]
	b.mu.Unlock()
	got, err := base64.StdEncoding.DecodeString(st.initImage)
	if err != nil {
		t.Fatalf("backend init_image is not base64: %v", err)
	}
	if len(got) != len(reference) || sha256.Sum256(got) != sha256.Sum256(reference) {
		t.Fatalf("reference arrived as %d bytes, want %d intact", len(got), len(reference))
	}
}

// A backend that exits while its job runs fails the job instead of polling
// until the job TTL, and the client's status poll says failed rather than 404.
func TestVideoAdapterBackendExitFailsJob(t *testing.T) {
	fastJobPolls(t)
	b := newSDCppBackend(t, time.Minute)
	r, h := sdcppJobRouter(t, b)

	id := submitJob(t, h, "vid")
	r.managed["vid"].Stop()
	waitJob(t, "job failed", func() bool {
		row, ok := jobRow(r, id)
		return ok && row.JobState == "failed"
	})
	for range 2 {
		rec := doJob(h, http.MethodGet, "/v1/videos/"+id, "")
		var v struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil ||
			v.ID != id || v.Status != "failed" || v.Error.Message == "" {
			t.Fatalf("poll after backend exit = %d %s, want a failed status with a readable error", rec.Code, rec.Body.String())
		}
	}
}

// The reported sequence: a job runs, its backend process goes away, and the
// client keeps polling. The watcher marks the job failed and every later
// status poll repeats that, instead of reading as "no such job".
func TestJobBackendExitReportsFailedStatus(t *testing.T) {
	fastJobPolls(t)
	b := newJobBackend(t, time.Minute)
	r, h := jobRouter(t, b, "test", "")

	id := submitJob(t, h, "vid")
	r.managed["vid"].Stop()
	waitJob(t, "job failed", func() bool {
		row, ok := jobRow(r, id)
		return ok && row.JobState == "failed"
	})
	for range 2 {
		rec := doJob(h, http.MethodGet, "/v1/videos/"+id, "")
		var v struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil ||
			v.ID != id || v.Status != "failed" || v.Error.Message == "" {
			t.Fatalf("status poll after backend exit = %d %s, want failed with a readable error", rec.Code, rec.Body.String())
		}
	}
}

// A status poll on a completed job must not decode the clip; only /content
// reads the result payload.
func TestVideoAdapterStatusSkipsResultPayload(t *testing.T) {
	fastJobPolls(t)
	b := newSDCppBackend(t, 100*time.Millisecond)
	b.badResult = true
	r, h := sdcppJobRouter(t, b)

	id := submitJob(t, h, "vid")
	waitJob(t, "job completed", func() bool {
		row, ok := jobRow(r, id)
		return ok && row.JobState == "completed"
	})
	rec := doJob(h, http.MethodGet, "/v1/videos/"+id, "")
	var v struct {
		Status string `json:"status"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.Status != "completed" {
		t.Fatalf("status on completed job = %d %s, want completed without decoding the result", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "b64_json") || strings.Contains(rec.Body.String(), "12345") {
		t.Fatalf("status response carries the result payload: %s", rec.Body.String())
	}
}

// A state the adapter does not know ends the job as failed, and the reply
// names the raw state.
func TestVideoStatusUnknownIsFailed(t *testing.T) {
	for _, c := range [][2]string{
		{"", "queued"}, {"queued", "queued"}, {"pending", "queued"},
		{"generating", "in_progress"}, {"processing", "in_progress"},
		{"completed", "completed"},
		{"failed", "failed"}, {"cancelled", "failed"},
		{"aborted", "failed"}, {"timeout", "failed"},
	} {
		if got := videoStatus(c[0]); got != c[1] {
			t.Errorf("videoStatus(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	doc := videoRecord(&sdcppJobRecord{ID: "job_1", Status: "aborted"}, "vid", "", 0)
	errObj, ok := doc["error"].(map[string]any)
	if doc["status"] != "failed" || !ok {
		t.Fatalf("unknown state record = %v, want failed with an error", doc)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "aborted") {
		t.Fatalf("error message = %q, want the raw state", msg)
	}
}

// A large seed reaches the backend as the digits it was submitted with.
func TestVideoAdapterSeedKeepsPrecision(t *testing.T) {
	b := newSDCppBackend(t, time.Minute)
	_, h := sdcppJobRouter(t, b)

	rec := doJob(h, http.MethodPost, "/v1/videos", `{"prompt":"a paper kite","seed":12345678901234567890}`)
	var v struct {
		ID string `json:"id"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.ID == "" {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
	}
	b.mu.Lock()
	seed := b.jobs[v.ID].seed
	b.mu.Unlock()
	if seed != "12345678901234567890" {
		t.Fatalf("backend seed = %q, want the submitted digits", seed)
	}
}

// Eviction pops finished jobs in the order they ended, skips ones a DELETE
// already removed, and prunes those so the queue stays bounded.
func TestJobTableEvictionQueue(t *testing.T) {
	r := spilloverRouter(t, "start_port: 5900\njob_table_max: 2\n")
	t.Cleanup(r.Shutdown)
	tgt := Target{Peer: "alpha", PeerID: "vid"}
	add := func(id string) *job {
		j := &job{id: id, model: "vid", target: "alpha", tgt: tgt, base: "http://127.0.0.1:9", code: 200, created: time.Now()}
		r.recordJob(j)
		return j
	}
	del := func(j *job) { // what a successful DELETE does
		r.endJob(j, "cancelled")
		r.mu.Lock()
		delete(r.jobs, j.id)
		r.mu.Unlock()
	}
	for i := range 20 {
		del(add(fmt.Sprintf("deleted-%d", i)))
	}
	r.mu.Lock()
	n := len(r.jobsDone)
	r.mu.Unlock()
	if n > 5 { // pruned once over 2*job_table_max, then one more append
		t.Fatalf("finished-job queue holds %d entries for an empty table; stale ones should be pruned", n)
	}
	first, second := add("first"), add("second")
	r.endJob(second, "completed") // ends before first
	r.endJob(first, "completed")
	add("third") // full: second finished first, so it goes
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jobs["second"] != nil || r.jobs["first"] == nil || r.jobs["third"] == nil {
		t.Fatalf("want first and third kept and second dropped, got %v", slices.Collect(maps.Keys(r.jobs)))
	}
}

func TestJobIDFromPath(t *testing.T) {
	for _, c := range [][2]string{
		{"/v1/videos", ""},
		{"/v1/videos/", ""},
		{"/v1/videos/video_1", "video_1"},
		{"/v1/videos/video_1/content", "video_1"},
		{"/v1/audio/speech", ""},
	} {
		if got := jobID(c[0]); got != c[1] {
			t.Errorf("jobID(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

// holdBackWriter must not send the head until released.
func TestJobHoldBackWriterHoldsHead(t *testing.T) {
	rec := httptest.NewRecorder()
	hw := &holdBackWriter{ResponseWriter: rec}
	hw.WriteHeader(http.StatusAccepted)
	_, _ = hw.Write([]byte(`{"id":"x"}`))
	hw.Flush()
	if rec.Flushed || rec.Body.Len() != 0 {
		t.Fatal("held response reached the client early")
	}
	hw.release()
	if rec.Code != http.StatusAccepted || !bytes.Equal(rec.Body.Bytes(), []byte(`{"id":"x"}`)) {
		t.Fatalf("released = %d %q", rec.Code, rec.Body.String())
	}
}
