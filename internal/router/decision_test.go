package router

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

// kevRequest is a System One body as a Kev client sends it: a state and named,
// typed questions. All values are invented.
const kevRequest = `{"model":"kev-9b","state":"Order A-1001 arrived with a cracked lid; the customer asks for their money back.","questions":{"intent":{"type":"choice","options":["refund","replace","other"]},"urgent":{"type":"noul","instructions":"The customer needs an answer today."}}}`

// kevReply is a System One reply with calibration fields written the way a
// backend prints them (a long float, a 0.0), plus a reported latency.
const kevReply = `{"model":"kev-9b","answers":{"intent":{"type":"choice","choice":"refund","confidence":0.7812,"probabilities":{"refund":0.8500000000000001,"replace":0.15,"other":0.0}},"urgent":{"type":"noul","noul":0.98}},"usage":{"input_tokens":120,"output_tokens":0},"latency_ms":41.7}`

// kevStanza is the decision stanza every in-process test serves; extra is
// appended as YAML lines.
func kevStanza(extra string) string {
	return `
  - model_id: kev-9b
    command: "fake --port {port}"
    vram_mb: 0
    device: cpu
    api_type: decision
    aliases: [kev]
    health_check: /health
    spin_up_seconds: 5
    port: %d` + extra
}

// kevBackend answers /v1/systemone with status and body, counting the calls
// and keeping the last request body it was sent.
type kevBackend struct {
	status int
	body   string
	hits   atomic.Int32
	got    atomic.Value // string
}

func (b *kevBackend) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/health" {
		return
	}
	raw, _ := io.ReadAll(req.Body)
	b.got.Store(string(raw))
	b.hits.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(b.status)
	_, _ = io.WriteString(w, b.body)
}

func newKevBackend(status int, body string) *kevBackend {
	return &kevBackend{status: status, body: body}
}

func postDecision(h http.Handler, body, ct string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Three ways in reach the one decision stanza: its id, its alias, and no model
// at all (the /v1/systemone path default), even next to a chat stanza.
func TestDecisionRouteByModelAliasAndPath(t *testing.T) {
	kev := newKevBackend(http.StatusOK, kevReply)
	chat := newKevBackend(http.StatusTeapot, "wrong stanza")
	_, h := speechNode(t, "stanzas:"+kevStanza("")+`
  - model_id: chat-a
    command: "fake --port {port}"
    vram_mb: 0
    device: cpu
    port: %d`, kev, chat)
	noModel := strings.Replace(kevRequest, `"model":"kev-9b",`, "", 1)
	for _, body := range []string{kevRequest, strings.Replace(kevRequest, `"kev-9b"`, `"kev"`, 1), noModel} {
		if rec := postDecision(h, body, "application/json"); rec.Code != http.StatusOK || rec.Body.String() != kevReply {
			t.Fatalf("%s: status %d body %q", body[:20], rec.Code, rec.Body.String())
		}
	}
	if kev.hits.Load() != 3 || chat.hits.Load() != 0 {
		t.Fatalf("kev hits %d, chat hits %d; want 3 and 0", kev.hits.Load(), chat.hits.Load())
	}
}

// The backend is not up until the router spawns it: the first request starts
// it and gets a decision, and a second request sent while it loads waits for
// the same start instead of spawning again.
func TestDecisionKevColdStart(t *testing.T) {
	var spawned atomic.Int32
	var upAt atomic.Int64 // health turns 200 here: 300 ms after the spawn
	kev := newKevBackend(http.StatusOK, kevReply)
	backend := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" && (upAt.Load() == 0 || time.Now().UnixNano() < upAt.Load()) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		kev.ServeHTTP(w, req)
	})
	r, h := speechNode(t, "stanzas:"+kevStanza(""), backend)
	r.spawn = func(string, []string) (Proc, error) {
		spawned.Add(1)
		upAt.Store(time.Now().Add(300 * time.Millisecond).UnixNano())
		return newFakeProc(), nil
	}
	if r.managed["kev-9b"] != nil {
		t.Fatal("kev-9b is running before any request")
	}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := postDecision(h, kevRequest, "application/json")
			codes[i] = rec.Code
			if rec.Body.String() != kevReply {
				t.Errorf("request %d body %q", i, rec.Body.String())
			}
		}()
		time.Sleep(100 * time.Millisecond) // the second request lands mid-load
	}
	wg.Wait()
	if kev.hits.Load() != 2 {
		t.Fatalf("backend answered %d decisions, want 2", kev.hits.Load())
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("statuses %v, want both 200", codes)
	}
	if n := spawned.Load(); n != 1 {
		t.Fatalf("spawned %d times, want 1", n)
	}
}

// A backend that answers 200 with something that is not a System One reply is
// a 502 naming the model, never the junk and never a panic.
func TestDecisionMalformedReply(t *testing.T) {
	for _, junk := range []string{`<html>oops</html>`, `{"answers":"refund"}`, `{"model":"kev-9b"}`, ``} {
		kev := newKevBackend(http.StatusOK, junk)
		_, h := speechNode(t, "stanzas:"+kevStanza(""), kev)
		rec := postDecision(h, kevRequest, "application/json")
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "kev-9b") {
			t.Errorf("reply %q: status %d body %q, want 502 naming kev-9b", junk, rec.Code, rec.Body.String())
		}
	}
}

// A 402 or 403 from the backend reaches the client once: no retry on the
// backend and no walk to a peer that also lists the model.
func TestDecisionBillingNoRetryLoop(t *testing.T) {
	for _, code := range []int{http.StatusPaymentRequired, http.StatusForbidden} {
		kev := newKevBackend(code, `{"error":{"message":"credits exhausted"}}`)
		peer := newKevBackend(http.StatusOK, kevReply)
		peerSrv := httptest.NewServer(peer)
		r, h := speechNode(t, "stanzas:"+kevStanza("")+`
peers:
  - name: alpha
    kind: router
    base_url: http://127.0.0.1:9
    models: [kev-9b]`, kev)
		r.cfg().Peer("alpha").BaseURL = peerSrv.URL
		r.peers.Set("alpha", &Telemetry{Node: "alpha", LoadedModels: []LoadedModel{{ID: "kev-9b", Slots: 4}}})
		start := time.Now()
		rec := postDecision(h, kevRequest, "application/json")
		peerSrv.Close()
		if rec.Code != code || !strings.Contains(rec.Body.String(), "credits exhausted") {
			t.Fatalf("%d: client got %d %q", code, rec.Code, rec.Body.String())
		}
		if kev.hits.Load() != 1 || peer.hits.Load() != 0 {
			t.Fatalf("%d: backend hits %d, peer hits %d; want 1 and 0", code, kev.hits.Load(), peer.hits.Load())
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("%d: took %v", code, d)
		}
	}
}

// With every local slot busy, a peer that has kev-9b loaded with a free slot
// serves the decision; a peer-qualified id goes straight to that peer.
func TestDecisionPeerSpill(t *testing.T) {
	kev := newKevBackend(http.StatusOK, `{"answers":{}}`)
	var mu sync.Mutex
	var paths, models []string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var b struct{ Model string }
		_ = json.Unmarshal(raw, &b)
		mu.Lock()
		paths, models = append(paths, req.URL.Path), append(models, b.Model)
		mu.Unlock()
		if req.URL.Path == "/_router/load" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, kevReply)
	}))
	defer peer.Close()
	r, h := speechNode(t, "stanzas:"+kevStanza("")+`
peers:
  - name: alpha
    kind: router
    base_url: http://127.0.0.1:9
    models: [kev-9b]`, kev)
	r.cfg().Peer("alpha").BaseURL = peer.URL
	r.peers.Set("alpha", &Telemetry{Node: "alpha", LoadedModels: []LoadedModel{{ID: "kev-9b", Slots: 2, InFlight: 0}}})
	r.holdOccupancy("kev-9b")
	defer r.releaseOccupancy(Target{Local: "kev-9b"})

	for _, body := range []string{kevRequest, strings.Replace(kevRequest, `"kev-9b"`, `"alpha/kev-9b"`, 1)} {
		if rec := postDecision(h, body, "application/json"); rec.Code != http.StatusOK || rec.Body.String() != kevReply {
			t.Fatalf("%s: status %d body %q", body[:24], rec.Code, rec.Body.String())
		}
	}
	if kev.hits.Load() != 0 {
		t.Fatalf("local backend hit %d times while full", kev.hits.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"/_router/load", "/v1/systemone", "/_router/load", "/v1/systemone"}; strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Fatalf("peer saw %v, want %v", paths, want)
	}
	if models[3] != "kev-9b" {
		t.Fatalf("peer-qualified id reached the peer as %q, want kev-9b", models[3])
	}
}

// Probabilities, confidence and the reported latency reach the client byte for
// byte as the backend wrote them.
func TestDecisionCalibrationPassthrough(t *testing.T) {
	kev := newKevBackend(http.StatusOK, kevReply)
	_, h := speechNode(t, "stanzas:"+kevStanza(""), kev)
	rec := postDecision(h, kevRequest, "application/json")
	if rec.Code != http.StatusOK || rec.Body.String() != kevReply {
		t.Fatalf("status %d body %q, want the backend's bytes", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"refund":0.8500000000000001`, `"other":0.0`, `"confidence":0.7812`, `"noul":0.98`, `"latency_ms":41.7`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("reply lost %s", want)
		}
	}
}

// A client that asks for gzip still gets a good reply: the backend's gzipped
// answer is decoded before the shape check, not refused as malformed.
func TestDecisionGzipReply(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = io.WriteString(w, kevReply)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, kevReply)
		_ = zw.Close()
	})
	_, h := speechNode(t, "stanzas:"+kevStanza(""), backend)
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(kevRequest))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != kevReply {
		t.Fatalf("status %d body %q, want the decoded reply", rec.Code, rec.Body.String())
	}
}

// A null confidence is absent, not 0: it must not pin the row's lowest.
func TestDecisionStatsNullConfidence(t *testing.T) {
	d := decisionStats([]byte(kevRequest), []byte(`{"answers":{"intent":{"type":"choice","choice":"refund","confidence":0.78},"urgent":{"type":"noul","noul":0.98,"confidence":null}}}`))
	if d == nil || d.Confidence == nil || *d.Confidence != 0.78 {
		t.Fatalf("stats %+v, want confidence 0.78", d)
	}
	d = decisionStats(nil, []byte(`{"answers":{"urgent":{"type":"noul","noul":0.98,"confidence":null}}}`))
	if d == nil || d.Confidence != nil {
		t.Fatalf("stats %+v, want no confidence", d)
	}
}

// A System One body reaches the backend unchanged. A wrong envelope is a 400
// naming the model and the fault, and the backend never sees it.
func TestDecisionSystemOneShape(t *testing.T) {
	kev := newKevBackend(http.StatusOK, kevReply)
	_, h := speechNode(t, "stanzas:"+kevStanza(""), kev)
	if rec := postDecision(h, kevRequest, "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if got, _ := kev.got.Load().(string); got != kevRequest {
		t.Fatalf("backend got %q, want the client's bytes", got)
	}
	for _, tc := range []struct{ body, ct, want string }{
		{kevRequest, "text/plain", "application/json"},
		{`[1,2]`, "application/json", "JSON object"},
		{`{"model":"kev-9b","questions":{"q":{"type":"noul"}}}`, "application/json", "state"},
		{`{"model":"kev-9b","state":"x"}`, "application/json", "questions"},
		{`{"model":"kev-9b","state":"x","questions":[{"type":"noul"}]}`, "application/json", "questions"},
		{`{"model":"kev-9b","state":"x","questions":{}}`, "application/json", "questions"},
		{`{"model":"kev-9b","state":"x","questions":{"q":{"options":["a"]}}}`, "application/json", `"q"`},
	} {
		rec := postDecision(h, tc.body, tc.ct)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "kev-9b") || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s (%s): status %d body %q, want 400 naming kev-9b and %s", tc.body, tc.ct, rec.Code, rec.Body.String(), tc.want)
		}
	}
	if n := kev.hits.Load(); n != 1 {
		t.Fatalf("backend hit %d times, want only the good request", n)
	}
}

// buildFakeModel compiles cmd/fake-model into a temp dir.
func buildFakeModel(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-model")
	out, err := exec.Command("go", "build", "-o", bin, "../../cmd/fake-model").CombinedOutput()
	if err != nil {
		t.Fatalf("build fake-model: %v\n%s", err, out)
	}
	return bin
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// The whole path with the kev-9b fixture: a real fake-model process started as
// `-model kev-9b`, cold, with a second request arriving while it loads, then by
// alias and by path default, each answered on /v1/systemone, and an Activity
// row carrying the model, the outcome and the confidence.
func TestDecisionE2EKev(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs cmd/fake-model")
	}
	bin := buildFakeModel(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
stanzas:
  - model_id: kev-9b
    command: "%s -model kev-9b -port {port} -health-delay 300ms"
    vram_mb: 0
    device: cpu
    api_type: decision
    decision_style: systemone
    aliases: [kev]
    spin_up_seconds: 20
    port: %d
`, bin, freePort(t))))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	var spawned atomic.Int32
	r.spawn = func(c string, env []string) (Proc, error) {
		spawned.Add(1)
		return spawnProc(c, env)
	}
	t.Cleanup(func() { _ = r.Unload("kev-9b") })
	h := NewHandler(r, NewLogger(io.Discard, false))

	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, 2)
	for i := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recs[i] = postDecision(h, kevRequest, "application/json")
		}()
		time.Sleep(50 * time.Millisecond) // the second request lands mid-load
	}
	wg.Wait()
	alias := strings.Replace(kevRequest, `"kev-9b"`, `"kev"`, 1)
	recs = append(recs, postDecision(h, alias, "application/json"),
		postDecision(h, strings.Replace(kevRequest, `"model":"kev-9b",`, "", 1), "application/json"))
	for i, rec := range recs {
		var reply struct {
			Model   string
			Answers map[string]struct {
				Type          string
				Choice        string
				Confidence    *float64
				Probabilities map[string]float64
				Noul          *float64
			}
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &reply) != nil {
			t.Fatalf("request %d: status %d body %q", i, rec.Code, rec.Body.String())
		}
		in, urgent := reply.Answers["intent"], reply.Answers["urgent"]
		if reply.Model != "kev-9b" || in.Type != "choice" || in.Choice == "" || in.Confidence == nil || len(in.Probabilities) != 3 || urgent.Type != "noul" || urgent.Noul == nil {
			t.Fatalf("request %d: reply %s is not a System One decision", i, rec.Body.String())
		}
	}
	if n := spawned.Load(); n != 1 {
		t.Fatalf("spawned %d times, want 1", n)
	}
	rows, _ := json.Marshal(r.Activity().List())
	var list []struct {
		Model    string
		Path     string
		Decision struct {
			Questions  int
			Outcome    string
			Confidence *float64
		}
	}
	if err := json.Unmarshal(rows, &list); err != nil || len(list) != 4 {
		t.Fatalf("activity rows %s", rows)
	}
	row := list[len(list)-1]
	if row.Model != "kev-9b" || row.Path != "/v1/systemone" || row.Decision.Questions != 2 ||
		!strings.Contains(row.Decision.Outcome, "intent=") || !strings.Contains(row.Decision.Outcome, "urgent=") || row.Decision.Confidence == nil {
		t.Fatalf("activity row %+v, want the model, outcome and confidence", row)
	}
}
