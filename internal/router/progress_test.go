package router

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

// loadingBackend answers health with 503 and the body from body(n), where n is
// the 0-based poll count. From okAfter polls on (when > 0) it answers 200 with
// the same body, so a progress field can sit beside a healthy status.
func loadingBackend(t *testing.T, okAfter int, body func(n int) string) int {
	t.Helper()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(polls.Add(1)) - 1
		if okAfter > 0 && n >= okAfter {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = io.WriteString(w, body(n))
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func progressBody(n int) string { return fmt.Sprintf(`{"progress": %d.5}`, n) }

// Spin-up is 1s and the backend becomes healthy after ~3s of 500ms polls, so
// success proves the deadline kept moving.
func TestSpinUpExtendsWhileProgressChanges(t *testing.T) {
	port := loadingBackend(t, 6, progressBody)
	m := testManaged(t, port, newFakeProc())
	m.stanza.SpinUpSeconds = 1
	start := time.Now()
	if _, err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if time.Since(start) < 2*time.Second {
		t.Fatalf("finished in %v, expected to outlast the 1s spin-up", time.Since(start))
	}
}

func TestSpinUpFailsAtDeadlineWhenProgressRepeats(t *testing.T) {
	// Progress moves once (0 -> 1), then sticks at 1.
	port := loadingBackend(t, 0, func(n int) string {
		if n == 0 {
			return `{"progress": 0}`
		}
		return `{"progress": 1}`
	})
	m := testManaged(t, port, newFakeProc())
	m.stanza.SpinUpSeconds = 1
	start := time.Now()
	_, err := m.Start()
	if err == nil || !strings.Contains(err.Error(), "health check timed out") {
		t.Fatalf("err = %v, want health check timed out", err)
	}
	// Last move is the second poll (~0.5s after start), so the deadline is
	// ~1.5s; a repeat must not push it further.
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("failed after %v, repeat progress must not extend the deadline", d)
	}
}

func TestSpinUpFailsAtDeadlineWithoutProgress(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"no field":      `{"status":"loading"}`,
		"not a number":  `{"progress":"half"}`,
		"null progress": `{"progress":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			port := loadingBackend(t, 0, func(int) string { return body })
			m := testManaged(t, port, newFakeProc())
			m.stanza.SpinUpSeconds = 1
			start := time.Now()
			_, err := m.Start()
			if err == nil || !strings.Contains(err.Error(), "health check timed out") {
				t.Fatalf("err = %v, want health check timed out", err)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("failed after %v, want about the 1s spin-up", d)
			}
		})
	}
}

// 200 wins even when the body carries a progress number.
func TestSpinUpHealthyWithProgressFieldCompletes(t *testing.T) {
	port := loadingBackend(t, 1, func(int) string { return `{"progress": 0.2}` })
	m := testManaged(t, port, newFakeProc())
	m.stanza.SpinUpSeconds = 1
	if _, err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.State() != StateRunning {
		t.Fatalf("state = %s, want running", m.State())
	}
}

// The same behaviour seen from the HTTP API: a client request through
// /upstream waits out a load longer than spin_up_seconds, or gets an error
// when progress stalls.
func TestHTTPUpstreamSpinUpFollowsProgress(t *testing.T) {
	cases := []struct {
		name    string
		okAfter int
		body    func(int) string
		wantOK  bool
	}{
		{"progress keeps moving", 6, progressBody, true},
		{"progress stalls", 0, func(int) string { return `{"progress": 3}` }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port := loadingBackend(t, tc.okAfter, tc.body)
			cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: slow-a1
    command: "fake --port {port}"
    vram_mb: 0
    health_check: /
    spin_up_seconds: 1
    port: %d
`, port)))
			if err != nil {
				t.Fatalf("config: %v", err)
			}
			r := New(cfg, NewLogger(io.Discard, false), "test")
			r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
			r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
			t.Cleanup(func() { _ = r.Unload("slow-a1") })
			h := NewHandler(r, NewLogger(io.Discard, false))

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/upstream/slow-a1/", nil))
			if got := rec.Code == http.StatusOK; got != tc.wantOK {
				t.Fatalf("status = %d, want ok=%v; body %s", rec.Code, tc.wantOK, rec.Body.String())
			}
		})
	}
}
