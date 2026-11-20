package router

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

func logTestServer(t *testing.T) (*httptest.Server, *Logger) {
	t.Helper()
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	logger := NewLogger(io.Discard, false)
	srv := httptest.NewServer(NewHandler(New(cfg, logger, "router-a"), logger))
	t.Cleanup(srv.Close)
	return srv, logger
}

func seedLogs(l *Logger) {
	l.Infof("proxy-one")
	l.Warnf("proxy-warn")
	l.Upstreamf("agents-a1", "%s", "up-a1")
	l.Upstreamf("embed", "%s", "up-embed")
	l.Meshf("mesh-one")
}

// openStream opens a stream and returns a scanner plus a closer. Reads block
// until a line is flushed, which is the point of the stream.
func openStream(t *testing.T, url string) (*bufio.Scanner, func()) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d", url, resp.StatusCode)
	}
	return bufio.NewScanner(resp.Body), func() { resp.Body.Close() }
}

func nextLine(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		if sc.Scan() {
			done <- sc.Text()
		} else {
			done <- "<eof>"
		}
	}()
	select {
	case s := <-done:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a streamed line")
		return ""
	}
}

func TestLogsPlainTextAndHTMLRedirect(t *testing.T) {
	srv, l := logTestServer(t)
	seedLogs(l)

	resp, err := http.Get(srv.URL + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	last := -1
	for _, want := range []string{"proxy-one", "proxy-warn", "up-a1", "up-embed", "mesh-one"} {
		i := strings.Index(string(body), want)
		if i <= last {
			t.Fatalf("%q missing or out of write order in %q", want, body)
		}
		last = i
	}

	req, _ := http.NewRequest("GET", srv.URL+"/logs", nil)
	req.Header.Set("Accept", "text/html")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusFound || r2.Header.Get("Location") != "/ui/" {
		t.Fatalf("html accept: status=%d location=%q", r2.StatusCode, r2.Header.Get("Location"))
	}
}

func TestLogStreamHistoryThenLive(t *testing.T) {
	srv, l := logTestServer(t)
	seedLogs(l)

	sc, closeFn := openStream(t, srv.URL+"/logs/stream")
	defer closeFn()
	for _, want := range []string{"proxy-one", "proxy-warn", "up-a1", "up-embed", "mesh-one"} {
		if got := nextLine(t, sc); !strings.Contains(got, want) {
			t.Fatalf("history: got %q want %q", got, want)
		}
	}
	l.Infof("live-line")
	if got := nextLine(t, sc); !strings.Contains(got, "live-line") {
		t.Fatalf("live: got %q", got)
	}
}

func TestLogStreamNoHistory(t *testing.T) {
	srv, l := logTestServer(t)
	seedLogs(l)

	sc, closeFn := openStream(t, srv.URL+"/logs/stream?no-history")
	defer closeFn()
	l.Infof("only-new")
	if got := nextLine(t, sc); !strings.Contains(got, "only-new") {
		t.Fatalf("no-history replayed history: got %q", got)
	}
}

func TestLogStreamFilters(t *testing.T) {
	cases := []struct {
		path string
		want []string
		live func(*Logger)
		next string
	}{
		{"/logs/stream/proxy", []string{"proxy-one", "[router:warn] proxy-warn"}, func(l *Logger) { l.Meshf("skip"); l.Infof("proxy-live") }, "proxy-live"},
		{"/logs/stream/upstream", []string{"[upstream] [agents-a1] up-a1", "[upstream] [embed] up-embed"}, func(l *Logger) { l.Infof("skip"); l.Upstreamf("embed", "up-live") }, "up-live"},
		{"/logs/stream/mesh", []string{"mesh-one"}, func(l *Logger) { l.Infof("skip"); l.Meshf("mesh-live") }, "mesh-live"},
		{"/logs/stream/agents-a1", []string{"up-a1"}, func(l *Logger) { l.Upstreamf("embed", "skip"); l.Upstreamf("agents-a1", "a1-live") }, "a1-live"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			srv, l := logTestServer(t)
			seedLogs(l)
			sc, closeFn := openStream(t, srv.URL+c.path)
			defer closeFn()
			for _, w := range c.want {
				if got := nextLine(t, sc); !strings.Contains(got, w) {
					t.Fatalf("history: got %q want %q", got, w)
				}
			}
			c.live(l)
			if got := nextLine(t, sc); !strings.Contains(got, c.next) {
				t.Fatalf("live: got %q want %q", got, c.next)
			}
		})
	}
}

// Extra copies of a model (max_instances) log as id#n. They belong in that
// model's stream, history and live, and keep the #n tag so they can be told apart.
func TestModelStreamIncludesCopies(t *testing.T) {
	srv, l := logTestServer(t)
	l.Upstreamf("agents-a1#1", "%s", "copy-history")
	sc, closeFn := openStream(t, srv.URL+"/logs/stream/agents-a1")
	defer closeFn()
	if got := nextLine(t, sc); !strings.HasSuffix(got, "[upstream] [agents-a1#1] copy-history") {
		t.Fatalf("history: got %q", got)
	}
	l.Upstreamf("embed#1", "%s", "skip")
	l.Upstreamf("agents-a1#2", "%s", "copy-live")
	if got := nextLine(t, sc); !strings.HasSuffix(got, "[upstream] [agents-a1#2] copy-live") {
		t.Fatalf("live: got %q", got)
	}
}

func TestLogStreamUnknownModel(t *testing.T) {
	srv, _ := logTestServer(t)
	resp, err := http.Get(srv.URL + "/logs/stream/no-such-model")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "unknown model id: no-such-model") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestRouterLogsJSONStillServed(t *testing.T) {
	srv, l := logTestServer(t)
	seedLogs(l)
	resp, err := http.Get(srv.URL + "/_router/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, key := range []string{`"entries"`, `"upstream"`, `"mesh"`, `"backend_metrics"`, "proxy-warn"} {
		if !strings.Contains(string(body), key) {
			t.Fatalf("missing %s in %s", key, body)
		}
	}
}

// The Logs page polls this snapshot when a proxy holds the live stream open;
// it carries what /logs/stream/{model_id} would replay, and keeps up.
func modelLogSnapshot(t *testing.T, url string) (string, []string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d", url, resp.StatusCode)
	}
	var body struct {
		Model string   `json:"model"`
		Lines []string `json:"lines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Model, body.Lines
}

func TestModelLogSnapshot(t *testing.T) {
	srv, l := logTestServer(t)
	seedLogs(l)
	model, lines := modelLogSnapshot(t, srv.URL+"/_router/logs/agents-a1")
	if model != "agents-a1" {
		t.Fatalf("model = %q", model)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "up-a1") || strings.Contains(joined, "up-embed") {
		t.Fatalf("lines = %q", lines)
	}
	l.Upstreamf("agents-a1", "%s", "a1-later")
	if _, lines := modelLogSnapshot(t, srv.URL+"/_router/logs/agents-a1"); !strings.Contains(strings.Join(lines, "\n"), "a1-later") {
		t.Fatalf("later lines = %q", lines)
	}
}

func TestModelLogSnapshotUnknownModel(t *testing.T) {
	srv, _ := logTestServer(t)
	resp, err := http.Get(srv.URL + "/_router/logs/no-such-model")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "unknown model id: no-such-model") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

// Ids may contain slashes, same as the stream path.
func TestModelLogSnapshotSlashID(t *testing.T) {
	cfg, err := config.Parse([]byte("stanzas:\n  - model_id: org/slash\n    command: \"x --port {port}\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	logger := NewLogger(io.Discard, false)
	logger.Upstreamf("org/slash", "%s", "slash-line")
	srv := httptest.NewServer(NewHandler(New(cfg, logger, "router-a"), logger))
	t.Cleanup(srv.Close)
	model, lines := modelLogSnapshot(t, srv.URL+"/_router/logs/org/slash")
	if model != "org/slash" || !strings.Contains(strings.Join(lines, "\n"), "slash-line") {
		t.Fatalf("model = %q lines = %q", model, lines)
	}
}

// chunkReader returns each chunk from one Read call, then EOF.
type chunkReader struct{ chunks []string }

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	c.chunks = c.chunks[1:]
	return n, nil
}

func TestCopyLogsBuffersPartialLines(t *testing.T) {
	l := NewLogger(io.Discard, false)
	m := newManaged(&config.Stanza{ModelID: "m1"}, 0, l)
	m.copyLogs(&chunkReader{[]string{"hel", "lo wor", "ld\nsecond\r\nthi", "rd (no newline at EOF)"}})
	got := l.RecentUpstream(0)
	want := []string{"[upstream] [m1] hello world", "[upstream] [m1] second", "[upstream] [m1] third (no newline at EOF)"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if !strings.HasSuffix(got[i], want[i]) {
			t.Fatalf("line %d: %q, want suffix %q", i, got[i], want[i])
		}
	}
}
