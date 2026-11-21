package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestActivityStreamSnapshotThenEntry(t *testing.T) {
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	srv := httptest.NewServer(NewHandler(r, logger))
	t.Cleanup(srv.Close)
	r.Activity().record(ActivityEntry{Model: "demo-old", Path: "/v1/chat/completions", Status: 200}, ActivityCapture{}, nil, "")

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

	start := time.Now()
	r.Activity().record(ActivityEntry{Model: "demo-new", Path: "/v1/chat/completions", Status: 200, DurationMs: 42}, ActivityCapture{}, nil, "")
	name, data = readEvent(t, br)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("entry took %v", d)
	}
	if name != "entry" {
		t.Fatalf("second event %q %s", name, data)
	}

	// The entry is byte-for-byte the row the list endpoint serves.
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

// A request listed as in progress in the snapshot later arrives as an entry
// event under the same id, so the client never shows it running forever.
func TestActivityStreamInProgressThenEntry(t *testing.T) {
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

	r.Activity().record(ActivityEntry{ID: run.ID, Model: "demo-slow", Path: "/v1/chat/completions", Status: 200}, ActivityCapture{}, nil, "")
	name, data = readEvent(t, br)
	var e ActivityEntry
	if name != "entry" || json.Unmarshal([]byte(data), &e) != nil || e.ID != run.ID {
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
			a.record(ActivityEntry{Model: "demo"}, ActivityCapture{}, nil, "")
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
