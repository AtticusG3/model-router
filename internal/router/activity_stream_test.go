package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"model-router/internal/config"
)

// readEvent reads one SSE event and returns its name and data.
func readEvent(t *testing.T, br *bufio.Reader) (string, string) {
	t.Helper()
	var name, data string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			return name, data
		case strings.HasPrefix(line, "event: "):
			name = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			data = line[len("data: "):]
		}
	}
}

// readUntil skips events until one named want, and returns it.
func readUntil(t *testing.T, br *bufio.Reader, want string) (string, string) {
	t.Helper()
	for {
		if name, data := readEvent(t, br); name == want {
			return name, data
		}
	}
}

func TestActivityStreamSnapshotThenDone(t *testing.T) {
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	srv := httptest.NewServer(NewHandler(r, logger))
	t.Cleanup(srv.Close)
	r.Activity().record(ActivityEntry{Model: "demo-old", Path: "/v1/chat/completions", Status: 200}, ActivityCapture{}, nil, "", [2][]preview{})

	resp, err := http.Get(srv.URL + "/_router/activity/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for k, want := range map[string]string{"Content-Type": "text/event-stream", "X-Accel-Buffering": "no", "Cache-Control": "no-cache"} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	br := bufio.NewReader(resp.Body)
	name, data := readEvent(t, br)
	var snap struct {
		Entries    []ActivityEntry   `json:"entries"`
		InProgress []InProgressEntry `json:"in_progress"`
	}
	if name != "snapshot" || json.Unmarshal([]byte(data), &snap) != nil || len(snap.Entries) != 1 || snap.Entries[0].Model != "demo-old" {
		t.Fatalf("first event %q %s", name, data)
	}

	if name, data = readEvent(t, br); name != "progress" || data != `{"in_progress":[]}` {
		t.Fatalf("second event %q %s, want an empty progress", name, data)
	}
	start := time.Now()
	r.Activity().record(ActivityEntry{Model: "demo-new", Path: "/v1/chat/completions", Status: 200, DurationMs: 42}, ActivityCapture{}, nil, "", [2][]preview{})
	name, data = readEvent(t, br)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("done took %v", d)
	}
	if name != "done" {
		t.Fatalf("third event %q %s", name, data)
	}

	// The done row is byte-for-byte the row the list endpoint serves.
	list, err := http.Get(srv.URL + "/_router/activity")
	if err != nil {
		t.Fatal(err)
	}
	defer list.Body.Close()
	var payload struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.NewDecoder(list.Body).Decode(&payload); err != nil || len(payload.Entries) != 2 {
		t.Fatalf("list: %v %d", err, len(payload.Entries))
	}
	if !bytes.Equal(payload.Entries[0], []byte(data)) {
		t.Fatalf("entry %s, list row %s", data, payload.Entries[0])
	}
}

// A request listed as in progress in the snapshot later arrives as a done
// event under the same id, so the client never shows it running forever.
func TestActivityStreamInProgressThenDone(t *testing.T) {
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	srv := httptest.NewServer(NewHandler(r, logger))
	t.Cleanup(srv.Close)
	run := r.Activity().begin(InProgressEntry{Model: "demo-slow", Method: "POST", Path: "/v1/chat/completions", Started: time.Now()})

	resp, err := http.Get(srv.URL + "/_router/activity/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	name, data := readEvent(t, br)
	var snap struct {
		InProgress []InProgressEntry `json:"in_progress"`
	}
	if name != "snapshot" || json.Unmarshal([]byte(data), &snap) != nil || len(snap.InProgress) != 1 || snap.InProgress[0].ID != run.ID {
		t.Fatalf("snapshot %q %s, want in_progress id %d", name, data, run.ID)
	}

	r.Activity().record(ActivityEntry{ID: run.ID, Model: "demo-slow", Path: "/v1/chat/completions", Status: 200}, ActivityCapture{}, nil, "", [2][]preview{})
	name, data = readUntil(t, br, "done")
	var e ActivityEntry
	if name != "done" || json.Unmarshal([]byte(data), &e) != nil || e.ID != run.ID {
		t.Fatalf("event %q %s, want entry id %d", name, data, run.ID)
	}
}

// A reader that never drains is dropped (its channel closed); record carries on.
func TestActivityStalledSubscriberDoesNotBlockRecord(t *testing.T) {
	a := NewActivityLog()
	_, live, cancel := a.Subscribe()
	defer cancel()
	done := make(chan struct{})
	go func() {
		for range 500 {
			a.record(ActivityEntry{Model: "demo"}, ActivityCapture{}, nil, "", [2][]preview{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("record blocked on a stalled subscriber")
	}
	n := 0
	for range live {
		n++
	}
	if n == 0 || n >= 500 {
		t.Fatalf("stalled subscriber got %d rows, want its buffer then a close", n)
	}
}

// fastProgress shortens the stream's progress interval for one test.
func fastProgress(t *testing.T) {
	old := activityProgressEvery
	activityProgressEvery = 20 * time.Millisecond
	t.Cleanup(func() { activityProgressEvery = old })
}

// openActivityStream opens the Activity stream with a deadline, so a missing
// event fails the read instead of hanging the test. Named apart from the Logs
// stream's openStream helper in logstream_test.go (same package).
func openActivityStream(t *testing.T, url string) *bufio.Reader {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/_router/activity/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return bufio.NewReader(resp.Body)
}

type streamProgressEvent struct {
	InProgress []struct {
		InProgressEntry
		Node string `json:"node"`
	} `json:"in_progress"`
}

// A slow request shows in progress events from its start, through its first
// token, then ends with one done event under the same id.
func TestActivityStreamProgressThenDone(t *testing.T) {
	fastProgress(t)
	port, release, hits := liveBackend(t)
	r, h := liveRouter(t, port, "bravo")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close) // after openActivityStream's cleanup closes the stream
	br := openActivityStream(t, srv.URL)
	readUntil(t, br, "snapshot")

	go func() {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody("Plan a fictional walk to Banksia Flat")))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	waitHit(t, hits)
	id, sawToken := 0, false
	for !sawToken {
		name, data := readEvent(t, br)
		if name == "done" {
			t.Fatalf("done before any token: %s", data)
		}
		var p streamProgressEvent
		if name != "progress" || json.Unmarshal([]byte(data), &p) != nil || len(p.InProgress) == 0 {
			continue
		}
		e := p.InProgress[0]
		if e.Node != "" || e.Model != "chat" || e.Path != "/v1/chat/completions" || e.Started.IsZero() {
			t.Fatalf("progress row = %+v", e)
		}
		id, sawToken = e.ID, e.Tokens > 0 && e.TTFTMs > 0
	}
	release <- struct{}{}
	_, data := readUntil(t, br, "done")
	var e ActivityEntry
	if json.Unmarshal([]byte(data), &e) != nil || e.ID != id || e.Status != http.StatusOK {
		t.Fatalf("done %s, want id %d", data, id)
	}
	if rows := r.Activity().List(); len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("rows = %+v", rows)
	}
}

// A peer's running request shows after one telemetry sync and turns into the
// peer's recent summary after the next. The peer's half of a request this node
// forwarded to it is left out: the local row already shows it.
func TestActivityStreamPeerRowsFromTelemetry(t *testing.T) {
	fastProgress(t)
	var mu sync.Mutex
	tel := Telemetry{Node: "bravo"}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, tel)
	}))
	defer peer.Close()
	cfg, err := config.Parse([]byte("start_port: 5900\npeers:\n  - name: bravo\n    kind: router\n    base_url: " + peer.URL + "\n    models: [chat]\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "alpha")
	srv := httptest.NewServer(NewHandler(r, NewLogger(io.Discard, false)))
	t.Cleanup(srv.Close) // after openActivityStream's cleanup closes the stream
	syncPeer := func(in []InProgressEntry, recent []RecentEntry) {
		mu.Lock()
		tel.InProgress, tel.Recent = in, recent
		mu.Unlock()
		NewPeerSyncer(r.PeerList, r.peers, time.Minute, nil).syncOnce()
	}

	started := time.Now().Add(-3 * time.Second).UTC()
	fwd := r.Activity().begin(InProgressEntry{Model: "chat", Target: "bravo", Method: "POST", Path: "/v1/chat/completions", Started: started})
	syncPeer([]InProgressEntry{
		{ID: 7, Model: "chat", Method: "POST", Path: "/v1/chat/completions", Started: started, TTFTMs: 120, Tokens: 3},
		{ID: 8, Model: "chat", Method: "POST", Path: "/v1/chat/completions", Started: started, FromPeer: "alpha", Target: "bravo"},
	}, nil)
	br := openActivityStream(t, srv.URL)
	readUntil(t, br, "snapshot")
	_, data := readUntil(t, br, "progress")
	var p streamProgressEvent
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range p.InProgress {
		got = append(got, e.Node+"/"+strconv.Itoa(e.ID))
	}
	if want := []string{"/" + strconv.Itoa(fwd.ID), "bravo/7"}; !slices.Equal(got, want) {
		t.Fatalf("progress rows %v, want %v (the forward once, as the local row)", got, want)
	}

	syncPeer(nil, []RecentEntry{
		{ID: 8, Model: "chat", Path: "/v1/chat/completions", Status: 200, FromPeer: "alpha", Target: "bravo"},
		{ID: 7, Model: "chat", Path: "/v1/chat/completions", Status: 200, DurationMs: 4200, Tokens: 42},
	})
	name, data := readEvent(t, br)
	var d struct {
		ActivityEntry
		Node    string `json:"node"`
		PeerURL string `json:"peer_url"`
	}
	if name != "done" || json.Unmarshal([]byte(data), &d) != nil || d.Node != "bravo" || d.ID != 7 || d.Status != 200 ||
		d.DurationMs != 4200 || d.Tokens.CompletionTokens != 42 || d.TTFTMs != 120 || d.Method != "POST" ||
		!d.Timestamp.Equal(started) || d.PeerURL != peer.URL {
		t.Fatalf("event %q %s, want bravo's summary of id 7", name, data)
	}
	// No done for id 8, which was never shown; the next event drops bravo/7.
	if name, data = readEvent(t, br); name != "progress" || json.Unmarshal([]byte(data), &p) != nil || len(p.InProgress) != 1 || p.InProgress[0].ID != fwd.ID {
		t.Fatalf("event %q %s, want progress with only the local row", name, data)
	}
}

// A request one fresh peer forwarded to another shows once, as the sender's
// row: the receiver's half (from_peer set to the sender) is left out. When the
// sender is not a fresh peer here, the receiver's row is all there is, so it stays.
func TestStreamProgressPeerToPeerForwardShowsOnce(t *testing.T) {
	cfg, err := config.Parse([]byte("start_port: 5900\npeers:\n  - name: bravo\n    kind: router\n    base_url: http://bravo.invalid\n    models: [chat]\n  - name: charlie\n    kind: router\n    base_url: http://charlie.invalid\n    models: [chat]\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "alpha")
	r.peers.Set("bravo", &Telemetry{Node: "bravo", InProgress: []InProgressEntry{{ID: 4, Model: "chat", Target: "charlie"}}})
	r.peers.Set("charlie", &Telemetry{Node: "charlie", InProgress: []InProgressEntry{
		{ID: 9, Model: "chat", FromPeer: "bravo"},
		{ID: 10, Model: "chat", FromPeer: "delta"},
	}})
	rows, _ := r.streamProgress(map[string]liveRow{})
	var got []string
	for _, e := range rows {
		got = append(got, e.Node+"/"+strconv.Itoa(e.ID))
	}
	if want := []string{"bravo/4", "charlie/10"}; !slices.Equal(got, want) {
		t.Fatalf("rows %v, want %v", got, want)
	}
}
