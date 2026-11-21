package router

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

// liveBackend streams one chat token, then holds every request until the test
// sends on release (or the request goes away). Each request reports its path
// on hits once its body is read. A chat body naming "fail" ends in a 500.
func liveBackend(t *testing.T) (port int, release chan struct{}, hits chan string) {
	t.Helper()
	release, hits = make(chan struct{}), make(chan string, 16)
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			return
		}
		body, _ := io.ReadAll(req.Body)
		hits <- req.URL.Path
		wait := func() {
			select {
			case <-release:
			case <-req.Context().Done():
			case <-stop:
			}
		}
		switch {
		case req.URL.Path != "/v1/chat/completions":
			wait()
			_, _ = w.Write([]byte(`{}`))
		case strings.Contains(string(body), "fail"):
			wait()
			http.Error(w, "fictional backend fault", http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Kia\"}}]}\n\n")
			w.(http.Flusher).Flush()
			wait()
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) }) // runs first, so Close is not held up
	return srv.Listener.Addr().(*net.TCPAddr).Port, release, hits
}

// liveRouter serves stanza chat from the live backend on port.
func liveRouter(t *testing.T, port int, node string) (*Router, http.Handler) {
	t.Helper()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: chat
    command: "fake --port {port}"
    vram_mb: 0
    device: cpu
    api_type: chat
    health_check: /health
    spin_up_seconds: 5
    port: %d
`, port)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), node)
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	t.Cleanup(func() { _ = r.Unload("chat") })
	return r, NewHandler(r, NewLogger(io.Discard, false))
}

func waitHit(t *testing.T, hits chan string) string {
	t.Helper()
	select {
	case p := <-hits:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the backend")
		return ""
	}
}

func chatBody(prompt string) string {
	return `{"model":"chat","stream":true,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"` + prompt + `"}]}`
}

// A streamed request alpha forwards to bravo shows in progress on both nodes,
// in their telemetry and in each other's peer cache after one sync, then turns
// into a finished row under the same id.
func TestInProgressAcrossMesh(t *testing.T) {
	port, release, hits := liveBackend(t)
	rB, hB := liveRouter(t, port, "bravo")
	srvB := httptest.NewServer(hB)
	defer srvB.Close()

	cfgA, err := config.Parse([]byte("start_port: 5900\npeers:\n  - name: bravo\n    kind: router\n    base_url: " + srvB.URL + "\n    models: [chat]\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	rA := New(cfgA, NewLogger(io.Discard, false), "alpha")
	rA.peers.Set("bravo", &Telemetry{Node: "bravo"})
	srvA := httptest.NewServer(NewHandler(rA, NewLogger(io.Discard, false)))
	defer srvA.Close()

	prompt := "Plan a fictional drive to Wattleby"
	resp, err := http.Post(srvA.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody(prompt)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	waitHit(t, hits)
	if line, _ := bufio.NewReader(resp.Body).ReadString('\n'); !strings.Contains(line, "Kia") {
		t.Fatalf("first line = %q", line)
	}

	var onA, onB InProgressEntry
	waitJob(t, "first token on both nodes", func() bool {
		a, b := rA.Activity().InProgress(), rB.Activity().InProgress()
		if len(a) != 1 || len(b) != 1 {
			return false
		}
		onA, onB = a[0], b[0]
		return onA.Tokens > 0 && onB.Tokens > 0
	})
	if onA.Model != "chat" || onA.Target != "bravo" || onA.FromPeer != "" || onA.Path != "/v1/chat/completions" ||
		onA.Started.IsZero() || onA.Prompt != prompt || onA.TTFTMs < 1 {
		t.Fatalf("alpha in progress = %+v", onA)
	}
	// Bravo serves it from its own backend, so its target is itself, as on any local row.
	if onB.Model != "chat" || onB.FromPeer != "alpha" || onB.Target != "bravo" || onB.Prompt != prompt || onB.TTFTMs < 1 {
		t.Fatalf("bravo in progress = %+v", onB)
	}

	var list struct {
		InProgress []InProgressEntry `json:"in_progress"`
	}
	if rec := doJob(NewHandler(rA, NewLogger(io.Discard, false)), http.MethodGet, "/_router/activity", ""); json.Unmarshal(rec.Body.Bytes(), &list) != nil ||
		len(list.InProgress) != 1 || list.InProgress[0].ID != onA.ID {
		t.Fatalf("alpha /_router/activity in_progress = %+v", list.InProgress)
	}

	// One sync each way: bravo reads alpha's telemetry, alpha reads bravo's.
	cfgB := &config.Config{Peers: []config.Peer{{Name: "alpha", Kind: "router", BaseURL: srvA.URL}}}
	NewPeerSyncer(func() *config.Config { return cfgB }, rB.peers, time.Minute, nil).syncOnce()
	NewPeerSyncer(rA.cfg, rA.peers, time.Minute, nil).syncOnce()
	if got := rB.peers.Snapshot()["alpha"]; got == nil || got.Node != "alpha" || len(got.InProgress) != 1 ||
		got.InProgress[0].ID != onA.ID || got.InProgress[0].Target != "bravo" {
		t.Fatalf("bravo's cache of alpha = %+v", got)
	}
	if got := rA.peers.Snapshot()["bravo"]; got == nil || len(got.InProgress) != 1 ||
		got.InProgress[0].ID != onB.ID || got.InProgress[0].FromPeer != "alpha" {
		t.Fatalf("alpha's cache of bravo = %+v", got)
	}

	release <- struct{}{}
	_, _ = io.Copy(io.Discard, resp.Body)
	waitJob(t, "both nodes finished", func() bool {
		return len(rA.Activity().InProgress()) == 0 && len(rB.Activity().InProgress()) == 0
	})
	if rows := rA.Activity().List(); len(rows) != 1 || rows[0].ID != onA.ID || rows[0].Target != "bravo" {
		t.Fatalf("alpha rows = %+v", rows)
	}
	if rows := rB.Activity().List(); len(rows) != 1 || rows[0].ID != onB.ID || rows[0].FromPeer != "alpha" {
		t.Fatalf("bravo rows = %+v", rows)
	}
	tel := rB.TelemetrySnapshot()
	if len(tel.InProgress) != 0 || len(tel.Recent) != 1 || tel.Recent[0].ID != onB.ID ||
		tel.Recent[0].FromPeer != "alpha" || tel.Recent[0].Status != http.StatusOK {
		t.Fatalf("bravo telemetry after = in_progress %+v recent %+v", tel.InProgress, tel.Recent)
	}
}

// A request leaves the in-progress list when the backend fails and when the
// client goes away mid-stream.
func TestInProgressEndsOnErrorAndDisconnect(t *testing.T) {
	port, release, hits := liveBackend(t)
	r, h := liveRouter(t, port, "bravo")
	srv := httptest.NewServer(h)
	defer srv.Close()
	running := func() int { return len(r.Activity().InProgress()) }

	code := make(chan int)
	go func() { code <- doJob(h, http.MethodPost, "/v1/chat/completions", chatBody("fail")).Code }()
	waitHit(t, hits)
	waitJob(t, "failing request in progress", func() bool { return running() == 1 })
	release <- struct{}{}
	if c := <-code; c != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", c)
	}
	if n := running(); n != 0 {
		t.Fatalf("in progress after error = %d", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(chatBody("hold")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	waitHit(t, hits)
	waitJob(t, "stream in progress", func() bool { return running() == 1 })
	cancel()
	resp.Body.Close()
	waitJob(t, "disconnected stream gone", func() bool { return running() == 0 })
}

// Proxied non-generation calls, metrics, health and the UI never show as in
// progress or in telemetry.
func TestInProgressSkipsNonGenerationRequests(t *testing.T) {
	port, release, hits := liveBackend(t)
	r, h := liveRouter(t, port, "bravo")
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/props?model=chat", ""},
		{http.MethodGet, "/web/sprite.svg?model=chat", ""},
		{http.MethodPost, "/tokenize", `{"model":"chat","content":"Wattleby"}`},
	} {
		done := make(chan struct{})
		go func() { doJob(h, c.method, c.path, c.body); close(done) }()
		waitHit(t, hits)
		if got := r.Activity().InProgress(); len(got) != 0 {
			t.Errorf("%s %s in progress: %+v", c.method, c.path, got)
		}
		release <- struct{}{}
		<-done
	}
	for _, path := range []string{"/metrics", "/health", "/ui/"} {
		doJob(h, http.MethodGet, path, "")
	}
	if tel := r.TelemetrySnapshot(); len(tel.InProgress) != 0 || len(tel.Recent) != 0 {
		t.Fatalf("telemetry in_progress %+v recent %+v, want none", tel.InProgress, tel.Recent)
	}
}

// Telemetry from a node without the in-progress fields still parses.
func TestOldTelemetryParses(t *testing.T) {
	var tel Telemetry
	old := `{"node":"charlie","gpus":[],"loaded_models":[{"id":"chat","freshness":"fresh","gpu":0,"slots":1,"in_flight":1}],"timestamp":1700000000}`
	if err := json.Unmarshal([]byte(old), &tel); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tel.Node != "charlie" || len(tel.LoadedModels) != 1 || tel.InProgress != nil || tel.Recent != nil {
		t.Fatalf("telemetry = %+v", tel)
	}
}

func TestPromptPreview(t *testing.T) {
	long := strings.Repeat("é", 130)
	for _, c := range []struct{ body, want string }{
		{chatBody("last message wins"), "last message wins"},
		{`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"what is this"}]}]}`, "what is this"},
		{`{"prompt":"a lighthouse at dusk"}`, "a lighthouse at dusk"},
		{`{"prompt":["", "second prompt"]}`, "second prompt"},
		{`{"input":"read this aloud","voice":"alloy"}`, "read this aloud"},
		{`{"prompt":"` + long + `"}`, strings.Repeat("é", 120)},
		{`--boundary` + "\r\n" + `Content-Disposition: form-data; name="file"`, ""},
	} {
		if got := promptPreview([]byte(c.body)); got != c.want {
			t.Errorf("promptPreview(%.40s) = %q, want %q", c.body, got, c.want)
		}
	}
}
