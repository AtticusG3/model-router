package router

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

type fakeProc struct {
	pid     int
	waitN   atomic.Int32
	termN   atomic.Int32
	killN   atomic.Int32
	mu      sync.Mutex
	alive   bool
	exited  bool
	waitCh  chan struct{}
	waitErr error
	stdout  io.Reader
	stderr  io.Reader
}

func newFakeProc() *fakeProc {
	return &fakeProc{
		pid:    4242,
		alive:  true,
		waitCh: make(chan struct{}),
		stdout: strings.NewReader(""),
		stderr: strings.NewReader(""),
	}
}

func (f *fakeProc) PID() int          { return f.pid }
func (f *fakeProc) Stdout() io.Reader { return f.stdout }
func (f *fakeProc) Stderr() io.Reader { return f.stderr }

func (f *fakeProc) Alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive
}

func (f *fakeProc) SignalTerm() error {
	f.termN.Add(1)
	f.exit(nil)
	return nil
}

func (f *fakeProc) Kill() error {
	f.killN.Add(1)
	f.exit(nil)
	return nil
}

func (f *fakeProc) Wait() error {
	f.waitN.Add(1)
	<-f.waitCh
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waitErr
}

func (f *fakeProc) exit(err error) {
	f.mu.Lock()
	if f.exited {
		f.mu.Unlock()
		return
	}
	f.exited = true
	f.alive = false
	f.waitErr = err
	f.mu.Unlock()
	close(f.waitCh)
}

func healthServer(t *testing.T) (port int, closeFn func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("httptest server addr is not TCP")
	}
	return addr.Port, srv.Close
}

func testManaged(t *testing.T, port int, fp *fakeProc) *Managed {
	t.Helper()
	m := newManaged(&config.Stanza{
		ModelID:       "test-model",
		Command:       "fake-backend",
		HealthCheck:   "/",
		SpinUpSeconds: 5,
	}, port, NewLogger(io.Discard, false))
	m.spawn = func(string, []string) (Proc, error) { return fp, nil }
	t.Cleanup(m.Stop)
	return m
}

func TestStartAbortsWhenProcessDies(t *testing.T) {
	fp := newFakeProc()
	fp.mu.Lock()
	fp.alive = false
	fp.mu.Unlock()
	m := testManaged(t, 1, fp)
	m.stanza.SpinUpSeconds = 8
	start := time.Now()
	_, err := m.Start()
	if err == nil {
		t.Fatal("expected start failure")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("must abort on dead process, not wait spin_up")
	}
	if !strings.Contains(err.Error(), "exited during start") {
		t.Fatalf("err = %v, want exited during start", err)
	}
}

func TestStartSuccess(t *testing.T) {
	port, _ := healthServer(t)
	fp := newFakeProc()
	m := testManaged(t, port, fp)

	if _, err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := m.State(); got != StateRunning {
		t.Fatalf("state = %s, want %s", got, StateRunning)
	}
	if fp.PID() != 4242 {
		t.Fatalf("pid = %d", fp.PID())
	}
}

func TestStopSignalsAndStops(t *testing.T) {
	port, _ := healthServer(t)
	fp := newFakeProc()
	m := testManaged(t, port, fp)

	if _, err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Stop()
	if got := m.State(); got != StateStopped {
		t.Fatalf("state = %s, want %s", got, StateStopped)
	}
	if fp.termN.Load() < 1 {
		t.Fatal("expected SignalTerm")
	}
	if fp.Alive() {
		t.Fatal("proc still alive after Stop")
	}
}

func TestWaitCalledOnce(t *testing.T) {
	port, _ := healthServer(t)
	fp := newFakeProc()
	m := testManaged(t, port, fp)

	if _, err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Stop()
	if n := fp.waitN.Load(); n != 1 {
		t.Fatalf("Wait called %d times, want 1", n)
	}
}

func TestAliveCheckRejectsDeadProc(t *testing.T) {
	port, _ := healthServer(t)
	fp := newFakeProc()
	fp.mu.Lock()
	fp.alive = false
	fp.mu.Unlock()
	m := testManaged(t, port, fp)

	_, err := m.Start()
	if err == nil {
		t.Fatal("expected Start to fail when proc is not alive")
	}
	if !strings.Contains(err.Error(), "process not alive") {
		t.Fatalf("err = %v, want process not alive", err)
	}
}

func TestStartAfterStopResetsStopOnce(t *testing.T) {
	port, _ := healthServer(t)
	var mu sync.Mutex
	var procs []*fakeProc
	m := newManaged(&config.Stanza{
		ModelID:       "test-model",
		Command:       "fake-backend",
		HealthCheck:   "/",
		SpinUpSeconds: 5,
	}, port, NewLogger(io.Discard, false))
	m.spawn = func(string, []string) (Proc, error) {
		fp := newFakeProc()
		mu.Lock()
		procs = append(procs, fp)
		mu.Unlock()
		return fp, nil
	}
	t.Cleanup(m.Stop)

	if _, err := m.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	m.Stop()
	if _, err := m.Start(); err != nil {
		t.Fatalf("second Start after Stop: %v", err)
	}
	if got := m.State(); got != StateRunning {
		t.Fatalf("state after second Start = %s, want %s", got, StateRunning)
	}
	m.Stop()
	if got := m.State(); got != StateStopped {
		t.Fatalf("state after second Stop = %s, want %s", got, StateStopped)
	}
	mu.Lock()
	n := len(procs)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("spawned %d procs, want 2", n)
	}
	if procs[1].waitN.Load() != 1 {
		t.Fatalf("second proc Wait called %d times, want 1", procs[1].waitN.Load())
	}
}

func TestCrashRestartAfterPriorStop(t *testing.T) {
	port, _ := healthServer(t)
	var mu sync.Mutex
	var procs []*fakeProc
	m := newManaged(&config.Stanza{
		ModelID:       "test-model",
		Command:       "fake-backend",
		HealthCheck:   "/",
		SpinUpSeconds: 5,
	}, port, NewLogger(io.Discard, false))
	m.spawn = func(string, []string) (Proc, error) {
		fp := newFakeProc()
		mu.Lock()
		procs = append(procs, fp)
		mu.Unlock()
		return fp, nil
	}
	t.Cleanup(m.Stop)

	if _, err := m.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	m.Stop()
	if _, err := m.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	mu.Lock()
	running := procs[len(procs)-1]
	mu.Unlock()
	running.exit(io.EOF)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(procs)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	n := len(procs)
	mu.Unlock()
	if n < 3 {
		t.Fatalf("crash after prior Stop did not restart (spawned %d, want >= 3); stopOnce likely spent", n)
	}
}
