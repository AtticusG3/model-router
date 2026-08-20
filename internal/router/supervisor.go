package router

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"model-router/internal/config"
)

// ProcessState mirrors llama-swap's lifecycle states.
type ProcessState string

const (
	StateStopped  ProcessState = "stopped"
	StateStarting ProcessState = "starting"
	StateRunning  ProcessState = "running"
	StateStopping ProcessState = "stopping"
	StateFailed   ProcessState = "failed"
)

// Managed is a supervised backend process for one stanza.
type Managed struct {
	stanza *config.Stanza
	logger *Logger

	mu       sync.Mutex
	state    ProcessState
	cmd      *exec.Cmd
	port     int
	gpuIdx   int
	lastUsed time.Time
	// inflight is the number of requests currently proxied to this backend.
	inflight int

	// restart bookkeeping
	restartAttempts int
	stopCh          chan struct{}
	stopOnce        sync.Once
}

// Logger is a minimal leveled logger to keep the router dependency-free.
// Recent entries are exposed to the local WebUI; the process stdout/stderr
// remains the durable source for systemd/journald.
type Logger struct {
	mu     sync.Mutex
	w      io.Writer
	verb   bool
	recent []string
}

const maxRecentLogs = 300

func NewLogger(w io.Writer, verbose bool) *Logger { return &Logger{w: w, verb: verbose} }

func (l *Logger) write(prefix, format string, args ...any) {
	line := prefix + fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recent = append(l.recent, line)
	if len(l.recent) > maxRecentLogs {
		l.recent = l.recent[len(l.recent)-maxRecentLogs:]
	}
	fmt.Fprintln(l.w, line)
}

func (l *Logger) Infof(format string, args ...any) {
	l.write("[router] ", format, args...)
}

func (l *Logger) Debugf(format string, args ...any) {
	if !l.verb {
		return
	}
	l.write("[router:debug] ", format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.write("[router:error] ", format, args...)
}

// RecentLogs returns the newest log entries, newest last.
func (l *Logger) RecentLogs(limit int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.recent) {
		limit = len(l.recent)
	}
	start := len(l.recent) - limit
	out := make([]string, limit)
	copy(out, l.recent[start:])
	return out
}

// State returns the current process state.
func (m *Managed) State() ProcessState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Touch records request activity (resets the idle TTL clock).
func (m *Managed) Touch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastUsed = time.Now()
	m.inflight++
}

// Done decrements in-flight count.
func (m *Managed) Done() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inflight > 0 {
		m.inflight--
	}
	m.lastUsed = time.Now()
}

// Inflight returns the current in-flight request count.
func (m *Managed) Inflight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inflight
}

// newManaged constructs a supervisor for a stanza. The port is the backend port.
func newManaged(s *config.Stanza, port int, logger *Logger) *Managed {
	return &Managed{
		stanza: s,
		logger: logger,
		port:   port,
		state:  StateStopped,
		stopCh: make(chan struct{}),
	}
}

// Start spawns the backend and waits for its health endpoint to return 200,
// or until the spin-up timeout. It returns the chosen GPU index on success.
//
// The process is deliberately NOT bound to the caller's context: a backend
// spawned for a peer load() must outlive the request that triggered it, or the
// process is killed as soon as the request completes. The health poll runs to
// the spin-up deadline even if the client disconnects (the model still becomes
// ready for the next request).
func (m *Managed) Start() (int, error) {
	m.mu.Lock()
	if m.state == StateRunning || m.state == StateStarting {
		m.mu.Unlock()
		return m.gpuIndex(), nil
	}
	m.state = StateStarting
	m.mu.Unlock()

	m.mu.Lock()
	m.gpuIdx = -1
	m.mu.Unlock()

	// Build the command. The stanza command is already macro-expanded.
	cmd := exec.Command("/bin/sh", "-c", m.stanza.Command)
	cmd.Env = append(os.Environ(), m.stanza.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.fail("stdout pipe: %v", err)
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.fail("stderr pipe: %v", err)
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		m.fail("spawn: %v", err)
		return -1, err
	}

	m.mu.Lock()
	m.cmd = cmd
	m.mu.Unlock()

	// Capture logs.
	go m.copyLogs(stdout)
	go m.copyLogs(stderr)

	m.logger.Infof("started %s (pid %d, port %d)", m.stanza.ModelID, cmd.Process.Pid, m.port)

	// Wait for health.
	spinUp := time.Duration(m.stanza.SpinUpSeconds) * time.Second
	deadline := time.Now().Add(spinUp)
	healthOK := false
	for time.Now().Before(deadline) {
		if err := m.health(); err == nil {
			healthOK = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !healthOK {
		// Give a couple extra seconds in case the endpoint is slow, then fail.
		m.logger.Errorf("health check for %s never returned 200 within %ds", m.stanza.ModelID, m.stanza.SpinUpSeconds)
		m.stop()
		m.fail("health check timed out")
		return -1, fmt.Errorf("health check timed out for %s", m.stanza.ModelID)
	}

	// Confirm the *spawned* process is actually alive. The health endpoint can
	// be served by a foreign process that grabbed our port (e.g. an orphaned
	// backend from a previous instance), which would otherwise let us mark a
	// dead child as healthy and restart-loop forever.
	m.mu.Lock()
	aliveProc := m.cmd != nil && m.cmd.Process != nil && m.cmd.Process.Signal(syscall.Signal(0)) == nil
	m.mu.Unlock()
	if !aliveProc {
		m.logger.Errorf("%s: health passed but spawned process is not alive (port %d held by another process?)", m.stanza.ModelID, m.port)
		m.stop()
		m.fail("process not alive after health check")
		return -1, fmt.Errorf("process not alive after health check for %s", m.stanza.ModelID)
	}

	m.mu.Lock()
	m.state = StateRunning
	m.lastUsed = time.Now()
	m.restartAttempts = 0
	m.mu.Unlock()

	// Record the GPU this model landed on (set by the router after Start).
	m.logger.Infof("%s healthy on port %d", m.stanza.ModelID, m.port)

	// Watch for unexpected exits and restart (crash/restart).
	go m.watch()

	return m.gpuIndex(), nil
}

func (m *Managed) gpuIndex() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gpuIdx
}

// SetGPU records which GPU this backend was admitted to.
func (m *Managed) SetGPU(idx int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gpuIdx = idx
}

// copyLogs feeds backend stdout/stderr to the debug logger.
func (m *Managed) copyLogs(r io.Reader) {
	buf := make([]byte, 16<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			m.logger.Debugf("[%s] %s", m.stanza.ModelID, strings.TrimRight(string(buf[:n]), "\n"))
		}
		if err != nil {
			return
		}
	}
}

// health polls the stanza's health endpoint.
func (m *Managed) health() error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", m.port, m.stanza.HealthCheck)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health %s -> %d", url, resp.StatusCode)
	}
	return nil
}

// watch restarts the backend if it dies unexpectedly while it is supposed to
// be running (crash/restart). Only restarts when the model is still wanted.
func (m *Managed) watch() {
	m.mu.Lock()
	cmd := m.cmd
	m.mu.Unlock()
	if cmd == nil {
		return
	}
	err := cmd.Wait()
	m.mu.Lock()
	wasRunning := m.state == StateRunning
	m.state = StateStopped
	m.mu.Unlock()

	if !wasRunning {
		return // intentional stop
	}
	m.logger.Errorf("%s exited unexpectedly: %v", m.stanza.ModelID, err)

	// Backoff restart, up to a cap. Keep trying while the router still wants
	// this model (i.e. it hasn't been explicitly unloaded).
	backoff := time.Second
	for {
		select {
		case <-m.stopCh:
			return
		case <-time.After(backoff):
		}
		m.mu.Lock()
		if m.state == StateStopping {
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		m.logger.Infof("restarting %s", m.stanza.ModelID)
		if _, err := m.Start(); err == nil {
			return
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// Stop gracefully stops the backend: SIGTERM to the process group, then SIGKILL
// after a short grace period.
func (m *Managed) Stop() {
	m.stop()
}

func (m *Managed) stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
	m.mu.Lock()
	cmd := m.cmd
	m.state = StateStopping
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		m.mu.Lock()
		m.state = StateStopped
		m.mu.Unlock()
		return
	}
	// Kill the process group so children (e.g. sd-server subprocesses) die too.
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		cmd.Process.Signal(syscall.SIGTERM)
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		if err == nil {
			syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			cmd.Process.Kill()
		}
		<-done
	}
	m.mu.Lock()
	m.state = StateStopped
	m.mu.Unlock()
}

func (m *Managed) fail(format string, args ...any) {
	m.mu.Lock()
	m.state = StateFailed
	m.mu.Unlock()
	m.logger.Errorf("[%s] %s", m.stanza.ModelID, fmt.Sprintf(format, args...))
}

// IsIdle reports whether the backend has been idle longer than its TTL.
// ttl <= 0 means never idle.
func (m *Managed) IsIdle(now time.Time) bool {
	ttl := m.stanza.IdleTTLSeconds
	if ttl <= 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StateRunning {
		return false
	}
	return now.Sub(m.lastUsed) > time.Duration(ttl)*time.Second
}
