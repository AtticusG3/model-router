package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

// speechFormats is the response_format to Content-Type table cmd/fake-model
// answers with.
var speechFormats = map[string]string{
	"mp3": "audio/mpeg", "opus": "audio/opus", "aac": "audio/aac",
	"flac": "audio/flac", "wav": "audio/wav", "pcm": "audio/pcm",
}

// fakeSpeech is a speech backend shaped like cmd/fake-model: audio bytes per
// response_format, four chunks 60 ms apart with stream: true, and a voice list.
func fakeSpeech(voices ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/audio/speech":
			var b struct {
				ResponseFormat string `json:"response_format"`
				Stream         bool   `json:"stream"`
			}
			_ = json.NewDecoder(req.Body).Decode(&b)
			w.Header().Set("Content-Type", speechFormats[b.ResponseFormat])
			if !b.Stream {
				_, _ = io.WriteString(w, "audio:"+b.ResponseFormat)
				return
			}
			fl := w.(http.Flusher)
			for i := range 4 {
				if i > 0 {
					time.Sleep(60 * time.Millisecond)
				}
				fmt.Fprintf(w, "chunk%d;", i)
				fl.Flush()
			}
		case "/v1/audio/voices":
			writeJSON(w, map[string]any{"object": "list", "data": voices})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}
}

// speechNode runs a router whose config is yaml with each %d filled by the port
// of the matching backend, in order.
func speechNode(t *testing.T, yaml string, backends ...http.Handler) (*Router, http.Handler) {
	t.Helper()
	var ports []any
	for _, b := range backends {
		srv := httptest.NewServer(b)
		t.Cleanup(srv.Close)
		ports = append(ports, srv.Listener.Addr().(*net.TCPAddr).Port)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(yaml, ports...)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	t.Cleanup(func() {
		for _, id := range cfg.StanzaIDs() {
			_ = r.Unload(id)
		}
	})
	return r, NewHandler(r, NewLogger(io.Discard, false))
}

// speechStanza is one CPU speech stanza; extra is appended as YAML lines.
func speechStanza(id, extra string) string {
	return `
  - model_id: ` + id + `
    command: "fake --port {port}"
    vram_mb: 0
    device: cpu
    api_type: speech
    health_check: /health
    spin_up_seconds: 5
    port: %d` + extra
}

func serve(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Every response_format comes back with the backend's bytes and its matching
// Content-Type.
func TestSpeechFormatsPassThrough(t *testing.T) {
	_, h := speechNode(t, "stanzas:"+speechStanza("tts-main", ""), fakeSpeech())
	for format, ct := range speechFormats {
		rec := serve(h, "POST", "/v1/audio/speech", `{"model":"tts-main","input":"hi","response_format":"`+format+`"}`)
		if rec.Code != http.StatusOK || rec.Body.String() != "audio:"+format {
			t.Fatalf("%s: status %d body %q", format, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != ct {
			t.Errorf("%s: Content-Type %q, want %q", format, got, ct)
		}
	}
}

// With stream: true the first chunk reaches the client well before the last:
// nothing between the backend and the client holds the audio back.
func TestSpeechStreamFirstChunkBeforeLast(t *testing.T) {
	_, h := speechNode(t, "stanzas:"+speechStanza("tts-main", ""), fakeSpeech())
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/audio/speech", "application/json",
		strings.NewReader(`{"input":"hi","response_format":"pcm","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []byte
	var first, last time.Time
	buf := make([]byte, 64)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if first.IsZero() {
				first = time.Now()
			}
			last = time.Now()
			got = append(got, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(got) != "chunk0;chunk1;chunk2;chunk3;" {
		t.Fatalf("body = %q", got)
	}
	if gap := last.Sub(first); gap < 100*time.Millisecond {
		t.Fatalf("first chunk only %v before the last, want at least 100ms; the stream was buffered", gap)
	}
}

// A model-less voice list asks the type default; once a second speech stanza
// is running its voices are merged in, each tagged with its model id. Listing
// voices never starts the second stanza.
func TestVoiceListMergesSpeechStanzas(t *testing.T) {
	r, h := speechNode(t, "admit_timeout_seconds: 5\nstanzas:"+
		speechStanza("tts-main", "\n    match: {path_default: true}")+speechStanza("tts-alt", ""),
		fakeSpeech("alto"), fakeSpeech("bass", "tenor"))

	rec := serve(h, "GET", "/v1/audio/voices", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"alto"`) {
		t.Fatalf("default list: status %d body %s", rec.Code, rec.Body.String())
	}
	if r.anyRunning(r.cfg().Stanza("tts-alt")) {
		t.Fatal("a voice list started a speech stanza that was not the default")
	}

	if _, err := r.Load("tts-alt"); err != nil {
		t.Fatal(err)
	}
	rec = serve(h, "GET", "/v1/audio/voices", "")
	var out struct {
		Data []struct{ ID, Model string } `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("merged list: status %d body %s", rec.Code, rec.Body.String())
	}
	var got []string
	for _, v := range out.Data {
		got = append(got, v.Model+"/"+v.ID)
	}
	slices.Sort(got)
	if want := []string{"tts-alt/bass", "tts-alt/tenor", "tts-main/alto"}; !slices.Equal(got, want) {
		t.Fatalf("voices = %v, want %v", got, want)
	}

	// ?model= still asks just that stanza, answered as the backend sent it.
	if rec := serve(h, "GET", "/v1/audio/voices?model=tts-alt", ""); !strings.Contains(rec.Body.String(), `"data":["bass","tenor"]`) {
		t.Fatalf("?model=tts-alt: %s", rec.Body.String())
	}
}

// Model-less voice requests (list, one voice, delete) go to the speech default.
func TestVoicesModelLessGoesToTypeDefault(t *testing.T) {
	cfg, err := config.Parse([]byte(fmt.Sprintf("stanzas:"+speechStanza("tts-main", ""), 5999)))
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []*http.Request{
		newReq("GET", "http://x/v1/audio/voices", "", ""),
		newReq("GET", "http://x/v1/audio/voices/narrator", "", ""),
		newReq("DELETE", "http://x/v1/audio/voices/narrator", "", ""),
	} {
		if ref, err := matchRequest(req, cfg, meshOf(cfg)); err != nil || ref.Local != "tts-main" {
			t.Errorf("%s %s: ref %+v err %v, want tts-main", req.Method, req.URL.Path, ref, err)
		}
	}
}

// A model-less request reaches the type default, and that stanza's
// path_rewrite and renameParams put it on the backend's own path and field.
func TestSpeechPathRewriteReachesBackendPath(t *testing.T) {
	got := make(chan string, 1)
	backend := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			return
		}
		b, _ := io.ReadAll(req.Body)
		got <- req.URL.Path + " " + string(b)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = io.WriteString(w, "RIFF")
	})
	r, h := speechNode(t, "stanzas:"+speechStanza("tts-native", `
    path_rewrite: [{from: /v1/audio/speech, to: /tts}]
    filters: {renameParams: {ref_audio: speaker_ref_b64}}`), backend)

	rec := serve(h, "POST", "/v1/audio/speech", `{"input":"hi","ref_audio":"QUJD"}`)
	if rec.Code != http.StatusOK || rec.Body.String() != "RIFF" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if g := <-got; g != `/tts {"input":"hi","speaker_ref_b64":"QUJD"}` {
		t.Fatalf("backend got %q", g)
	}
	// The activity row keeps the route the client asked for.
	if e := r.Activity().List(); len(e) != 1 || e[0].Path != "/v1/audio/speech" {
		t.Fatalf("activity = %+v", e)
	}
}

// slots caps the syntheses in the backend at once; a request over the cap
// waits for a free slot, and gets 503 with Retry-After when the admit timeout
// runs out first.
func TestSpeechSlotsQueueUpToAdmitTimeout(t *testing.T) {
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	backend := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			return
		}
		n := inFlight.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		<-release
		inFlight.Add(-1)
		_, _ = io.WriteString(w, "RIFF")
	})
	_, h := speechNode(t, "admit_timeout_seconds: 1\nstanzas:"+speechStanza("tts-main", "\n    slots: 1"), backend)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	first := make(chan int, 1)
	go func() { first <- serve(h, "POST", "/v1/audio/speech", `{"input":"one"}`).Code }()
	waitFor(t, "the first synthesis to reach the backend", func() bool { return inFlight.Load() == 1 })

	// The admit timeout runs out while the first holds the only slot.
	start := time.Now()
	rec := serve(h, "POST", "/v1/audio/speech", `{"input":"two"}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the cap: status %d Retry-After %q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if waited := time.Since(start); waited < 900*time.Millisecond {
		t.Fatalf("refused after %v, want a wait of about the 1s admit timeout", waited)
	}

	// A request that finds the slot freed in time is served after the first.
	second := make(chan int, 1)
	go func() { second <- serve(h, "POST", "/v1/audio/speech", `{"input":"three"}`).Code }()
	time.Sleep(200 * time.Millisecond)
	close(release)
	if a, b := <-first, <-second; a != http.StatusOK || b != http.StatusOK {
		t.Fatalf("statuses %d %d, want 200 200", a, b)
	}
	if p := peak.Load(); p != 1 {
		t.Fatalf("backend ran %d syntheses at once, want 1", p)
	}
}

// A slot wait that times out gets no second full admit budget from the sibling
// retry: the local path shares one deadline, so the client waits about one
// admit timeout and then gets the 503.
func TestSpeechSlotTimeoutNotDoubledBySiblingRetry(t *testing.T) {
	r, h := speechNode(t, "admit_timeout_seconds: 1\nstanzas:"+
		speechStanza("tts-main", "\n    slots: 1\n    max_instances: 2"), fakeSpeech())
	// Both instances are busy: the first holds the only slot, the second is
	// live (occupied) so the failed attempt has a sibling to retry on.
	r.mu.Lock()
	r.speaking["tts-main"] = 1
	r.occupancy["tts-main#1"] = 1
	r.mu.Unlock()
	t.Cleanup(func() {
		r.mu.Lock()
		r.speaking["tts-main"] = 0
		r.occupancy["tts-main#1"] = 0
		r.mu.Unlock()
	})

	start := time.Now()
	rec := serve(h, "POST", "/v1/audio/speech", `{"input":"hi"}`)
	waited := time.Since(start)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d Retry-After %q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if waited < 900*time.Millisecond || waited > 1600*time.Millisecond {
		t.Fatalf("waited %v, want about one 1s admit timeout, not two", waited)
	}
}
