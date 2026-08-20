package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"model-router/internal/config"
)

// Telemetry is the snapshot exchanged between peers.
type Telemetry struct {
	Node         string        `json:"node"`
	GPUs         []*GPUState   `json:"gpus"`
	LoadedModels []LoadedModel `json:"loaded_models"`
	Timestamp    int64         `json:"timestamp"`
}

// LoadedModel is one locally running backend as advertised to peers.
type LoadedModel struct {
	ID        string `json:"id"`
	Freshness string `json:"freshness"` // fresh | stale
	VramMB    int64  `json:"vram_mb,omitempty"`
	GPU       int    `json:"gpu"`
}

// GPUPoller runs nvidia-smi on an interval and feeds the ledger.
type GPUPoller struct {
	ledger *Ledger
	every  time.Duration
}

func NewGPUPoller(ledger *Ledger, every time.Duration) *GPUPoller {
	return &GPUPoller{ledger: ledger, every: every}
}

// Run polls until ctx is cancelled. One immediate poll happens first.
func (p *GPUPoller) Run(ctx context.Context) {
	p.PollOnce()
	t := time.NewTicker(p.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.PollOnce()
		}
	}
}

// PollOnce reads nvidia-smi and updates the ledger synchronously. Call this
// before preloading so admission control sees the GPU state at startup.
func (p *GPUPoller) PollOnce() {
	p.pollOnce()
}

// pollOnce reads nvidia-smi and updates the ledger.
func (p *GPUPoller) pollOnce() {
	gpus, err := queryGPUs()
	if err != nil {
		return
	}
	p.ledger.SetGPUs(gpus)
}

// queryGPUs reads GPU state from nvidia-smi. The SPEC allows nvidia-smi or
// pynvml; nvidia-smi keeps the binary dependency-free.
//
// It is resilient to a single bad GPU: one failing device (common with a
// flaky driver) must not blind the router to the healthy ones, so it queries
// each GPU by index and keeps whatever succeeds.
func queryGPUs() ([]*GPUState, error) {
	// First enumerate GPUs via -L (cheap, works even when a device is sick).
	listOut, err := exec.Command("nvidia-smi", "-L").Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi -L: %w", err)
	}
	var indices []int
	for _, line := range strings.Split(string(listOut), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "GPU ") {
			continue
		}
		var idx int
		if _, err := fmt.Sscanf(line, "GPU %d:", &idx); err == nil {
			indices = append(indices, idx)
		}
	}
	if len(indices) == 0 {
		return nil, fmt.Errorf("nvidia-smi -L: no GPUs enumerated")
	}

	var gpus []*GPUState
	var firstErr error
	for _, idx := range indices {
		out, err := exec.Command("nvidia-smi",
			"--id="+fmt.Sprintf("%d", idx),
			"--query-gpu=index,name,memory.total,memory.free",
			"--format=csv,noheader,nounits").Output()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("gpu %d: %w", idx, err)
			}
			continue
		}
		line := strings.TrimSpace(string(out))
		parts := strings.Split(line, ",")
		if len(parts) < 4 {
			continue
		}
		var total, free int64
		fmt.Sscanf(strings.TrimSpace(parts[2]), "%d", &total)
		fmt.Sscanf(strings.TrimSpace(parts[3]), "%d", &free)
		gpus = append(gpus, &GPUState{
			Index:        idx,
			Name:         strings.TrimSpace(parts[1]),
			TotalMB:      total,
			FreeMB:       free,
			LastSeenUnix: time.Now().Unix(),
		})
	}
	if len(gpus) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("nvidia-smi: no GPU data parsed")
	}
	return gpus, nil
}

// PeerCache holds cached telemetry for each peer with a timestamp.
type PeerCache struct {
	mu    sync.Mutex
	peers map[string]*peerEntry
	stale time.Duration
}

type peerEntry struct {
	Telemetry *Telemetry
	Seen      time.Time
	Err       string
}

func NewPeerCache(stale time.Duration) *PeerCache {
	return &PeerCache{
		peers: map[string]*peerEntry{},
		stale: stale,
	}
}

// Fresh reports whether the peer has telemetry newer than the staleness window.
func (c *PeerCache) Fresh(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.peers[name]
	if !ok || e.Telemetry == nil {
		return false
	}
	return time.Since(e.Seen) <= c.stale
}

// Set stores a peer snapshot.
func (c *PeerCache) Set(name string, t *Telemetry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peers[name] = &peerEntry{Telemetry: t, Seen: time.Now()}
}

// SetErr records a failed fetch.
func (c *PeerCache) SetErr(name string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peers[name] = &peerEntry{Err: err.Error(), Seen: time.Now()}
}

// Snapshot returns a copy of all cached telemetry (for /_router/status).
func (c *PeerCache) Snapshot() map[string]*Telemetry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]*Telemetry{}
	for k, e := range c.peers {
		if e.Telemetry != nil {
			out[k] = e.Telemetry
		}
	}
	return out
}

// PeerSyncer periodically pulls telemetry from router-kind peers.
type PeerSyncer struct {
	cfg    *config.Config
	cache  *PeerCache
	every  time.Duration
	client *http.Client
}

func NewPeerSyncer(cfg *config.Config, cache *PeerCache, every time.Duration) *PeerSyncer {
	return &PeerSyncer{
		cfg:    cfg,
		cache:  cache,
		every:  every,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

func (s *PeerSyncer) Run(ctx context.Context) {
	s.syncOnce()
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncOnce()
		}
	}
}

func (s *PeerSyncer) syncOnce() {
	for _, p := range s.cfg.Peers {
		if p.Kind != "router" {
			continue // openai peers have no control API
		}
		url := p.BaseURL + "/_router/telemetry"
		resp, err := s.client.Get(url)
		if err != nil {
			s.cache.SetErr(p.Name, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			s.cache.SetErr(p.Name, fmt.Errorf("telemetry %s -> %d", url, resp.StatusCode))
			continue
		}
		var t Telemetry
		if err := json.Unmarshal(body, &t); err != nil {
			s.cache.SetErr(p.Name, err)
			continue
		}
		s.cache.Set(p.Name, &t)
	}
}
