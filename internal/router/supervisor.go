package router

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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

// Managed is a supervised backend process for one instance of a stanza.
type Managed struct {
	stanza *config.Stanza
	logger *Logger
	// id is the instance key: the model id, or "model#n" for extra copies.
	id string
	// command and env are rendered for this instance's port and GPU.
	command  string
	env      []string
	proxyURL string

	mu     sync.Mutex
	state  ProcessState
	proc   Proc
	port   int
	gpuIdx int
	// spawn, if set, overrides the GOOS spawnProc. Tests inject a fake.
	spawn    spawner
	lastUsed time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	// waitDone is closed by the single Wait owner (watch) when the process exits.
	waitDone chan struct{}
	// startWait is closed when the in-flight Start attempt finishes. Joiners block on it.
	startWait chan struct{}
	startErr  error
	// shutdown is closed by the router on process shutdown. Nil in tests that
	// never shut down. A nil channel never fires in select.
	shutdown <-chan struct{}
}

// State returns the current process state.
func (m *Managed) State() ProcessState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// markUsed resets the idle-TTL stale clock. Pool occupancy lives on Router.
func (m *Managed) markUsed() {
	m.mu.Lock()
	m.lastUsed = time.Now()
	m.mu.Unlock()
}

// newManaged constructs a supervisor for a stanza. The port is the backend port.
func newManaged(s *config.Stanza, port int, logger *Logger) *Managed {
	return &Managed{
		stanza:   s,
		logger:   logger,
		id:       s.ModelID,
		command:  s.Command,
		env:      s.Env,
		proxyURL: s.Proxy,
		port:     port,
		state:    StateStopped,
		stopCh:   make(chan struct{}),
	}
}

// joinedStart is a Start error observed by a caller that waited on someone
// else's attempt. That caller must not release the reservation or stop the
// process; the owner does.
type joinedStart struct{ err error }

func (e joinedStart) Error() string { return e.err.Error() }
func (e joinedStart) Unwrap() error { return e.err }

// Start spawns the backend and waits for its health endpoint to return 200,
// or until the spin-up timeout. It returns the chosen GPU index on success.
//
// A second Start while this one is still spinning up waits for it instead of
// reporting success early. Callers (the upstream UI included) then proxy only
// once the backend is actually healthy.
//
// The process is deliberately NOT bound to the caller's context: a backend
// spawned for a peer load() must outlive the request that triggered it, or the
// process is killed as soon as the request completes. The health poll runs to
// the spin-up deadline even if the client disconnects (the model still becomes
// ready for the next request).
func (m *Managed) Start() (int, error) {
	m.mu.Lock()
	if m.state == StateRunning {
		idx := m.gpuIdx
		m.mu.Unlock()
		return idx, nil
	}
	if m.state == StateStarting {
		ch := m.startWait
		m.mu.Unlock()
		if ch != nil {
			<-ch
		}
		return m.joinedResult()
	}
	ch := make(chan struct{})
	m.startWait = ch
	m.startErr = nil
	m.state = StateStarting
	m.gpuIdx = -1
	m.stopCh = make(chan struct{})
	m.stopOnce = sync.Once{}
	m.proc = nil
	m.waitDone = nil
	spawn := m.spawn
	cmd, env := m.command, m.env
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.state != StateRunning && m.startErr == nil {
			m.startErr = fmt.Errorf("start failed for %s", m.id)
			m.state = StateFailed
		}
		m.mu.Unlock()
		close(ch)
	}()

	if spawn == nil {
		spawn = spawnProc
	}
	proc, err := spawn(cmd, env)
	if err != nil {
		return -1, m.fail(err)
	}

	m.mu.Lock()
	m.proc = proc
	m.waitDone = make(chan struct{})
	m.mu.Unlock()

	go m.copyLogs(proc.Stdout())
	go m.copyLogs(proc.Stderr())
	go m.watch()

	m.logger.Infof("started %s (pid %d, port %d)", m.id, proc.PID(), m.port)

	m.mu.Lock()
	stopCh := m.stopCh
	m.mu.Unlock()

	// Wait for health. Unload/evict closes stopCh so a non-generating
	// spin-up can be replaced instead of blocking admission for spin_up_seconds.
	spinUp := time.Duration(m.stanza.SpinUpSeconds) * time.Second
	deadline := time.Now().Add(spinUp)
	healthOK := false
	for time.Now().Before(deadline) {
		if err := m.health(); err == nil {
			healthOK = true
			break
		}
		m.mu.Lock()
		alive := m.proc != nil && m.proc.Alive()
		m.mu.Unlock()
		if !alive {
			return -1, m.fail(fmt.Errorf("exited during start for %s", m.id))
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-stopCh:
			timer.Stop()
			return -1, m.fail(fmt.Errorf("stopped during start for %s", m.id))
		case <-timer.C:
		}
	}
	if !healthOK {
		m.logger.Errorf("health check for %s never returned 200 within %ds", m.id, m.stanza.SpinUpSeconds)
		m.stop()
		return -1, m.fail(fmt.Errorf("health check timed out for %s", m.id))
	}

	// Confirm the *spawned* process is actually alive. The health endpoint can
	// be served by a foreign process that grabbed our port (e.g. an orphaned
	// backend from a previous instance), which would otherwise let us mark a
	// dead child as healthy and restart-loop forever.
	m.mu.Lock()
	aliveProc := m.proc
	m.mu.Unlock()
	if aliveProc == nil || !aliveProc.Alive() {
		m.logger.Errorf("%s: health passed but spawned process is not alive (port %d held by another process?)", m.id, m.port)
		m.stop()
		return -1, m.fail(fmt.Errorf("process not alive after health check for %s", m.id))
	}

	m.mu.Lock()
	m.state = StateRunning
	m.lastUsed = time.Now()
	m.mu.Unlock()

	// Record the GPU this model landed on (set by the router after Start).
	m.logger.Infof("%s healthy on port %d", m.id, m.port)

	return m.gpuIndex(), nil
}

func (m *Managed) joinedResult() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == StateRunning {
		return m.gpuIdx, nil
	}
	if m.startErr != nil {
		return -1, joinedStart{err: m.startErr}
	}
	return -1, joinedStart{err: fmt.Errorf("model failed to start")}
}

func (m *Managed) shuttingDown() bool {
	if m.shutdown == nil {
		return false
	}
	select {
	case <-m.shutdown:
		return true
	default:
		return false
	}
}

func (m *Managed) gpuIndex() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gpuIdx
}

// place renders the backend command for the GPU admission chose. A start
// already in flight keeps the command it spawned with.
func (m *Managed) place(gpu int) {
	cmd, env := m.stanza.Render(instanceIndex(m.id), gpu)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == StateStarting || m.state == StateRunning {
		return
	}
	m.command, m.env = cmd, env
}

// SetGPU records which GPU this backend was admitted to.
func (m *Managed) SetGPU(idx int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gpuIdx = idx
}

// copyLogs feeds backend stdout/stderr to the upstream log ring.
func (m *Managed) copyLogs(r io.Reader) {
	if r == nil {
		return
	}
	buf := make([]byte, 16<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			text := strings.TrimRight(string(buf[:n]), "\n")
			for _, line := range strings.Split(text, "\n") {
				line = strings.TrimRight(line, "\r")
				if line == "" {
					continue
				}
				if len(line) > 2000 {
					line = line[:2000] + "..."
				}
				m.logger.Upstreamf("[%s] %s", m.id, line)
			}
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

// watch is the single Wait owner. It restarts the backend if it dies
// unexpectedly while it is supposed to be running (crash/restart).
func (m *Managed) watch() {
	m.mu.Lock()
	proc := m.proc
	waitDone := m.waitDone
	m.mu.Unlock()
	if proc == nil {
		return
	}
	err := proc.Wait()
	if waitDone != nil {
		close(waitDone)
	}

	m.mu.Lock()
	wasRunning := m.state == StateRunning
	m.state = StateStopped
	m.mu.Unlock()

	if !wasRunning || m.shuttingDown() {
		return // intentional stop, or the process is shutting down
	}
	m.logger.Errorf("%s exited unexpectedly: %v", m.id, err)

	// Backoff restart, up to a cap. Keep trying while the router still wants
	// this model (i.e. it hasn't been explicitly unloaded).
	backoff := time.Second
	for {
		select {
		case <-m.stopCh:
			return
		case <-m.shutdown:
			return
		case <-time.After(backoff):
		}
		if m.shuttingDown() {
			return
		}
		m.mu.Lock()
		if m.state == StateStopping {
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		m.logger.Infof("restarting %s", m.id)
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
	proc := m.proc
	waitDone := m.waitDone
	m.state = StateStopping
	m.mu.Unlock()
	if proc == nil {
		m.mu.Lock()
		m.state = StateStopped
		m.mu.Unlock()
		return
	}
	_ = proc.SignalTerm()
	if waitDone == nil {
		m.mu.Lock()
		m.state = StateStopped
		m.mu.Unlock()
		return
	}
	select {
	case <-waitDone:
	case <-time.After(10 * time.Second):
		_ = proc.Kill()
		<-waitDone
	}
	m.mu.Lock()
	m.state = StateStopped
	m.mu.Unlock()
}

func (m *Managed) fail(err error) error {
	m.mu.Lock()
	m.state = StateFailed
	m.startErr = err
	m.mu.Unlock()
	m.logger.Errorf("[%s] %s", m.id, err.Error())
	return err
}

// IsStale reports whether a running backend has been idle longer than its TTL.
// ttl <= 0 means never stale (resident). Stale models stay loaded until a
// later admission needs their VRAM.
func (m *Managed) IsStale(now time.Time) bool {
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

func (m *Managed) setLastUsed(t time.Time) {
	m.mu.Lock()
	m.lastUsed = t
	m.mu.Unlock()
}
