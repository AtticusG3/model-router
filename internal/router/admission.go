package router

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"model-router/internal/config"
)

// GPUState is the live view of one GPU, refreshed by the telemetry poller.
type GPUState struct {
	Index                int    `json:"index"`
	Name                 string `json:"name"`
	TotalMB              int64  `json:"total_mb"`
	FreeMB               int64  `json:"free_mb"`
	FreeIfStaleEvictedMB int64  `json:"free_if_stale_evicted_mb,omitempty"`
	LastSeenUnix         int64  `json:"last_seen_unix"`
}

// Ledger is the sole authority over this node's GPU reservations. Peers only
// ever see our telemetry; they never mutate our ledger. This is what prevents
// split-brain double-booking (SPEC: Admission control).
type Ledger struct {
	mu   sync.Mutex
	gpus []*GPUState
	// reservations maps modelID -> {gpuIndex, vramMB} for models we have
	// reserved but that may not yet show up in nvidia-smi (spinning up) or
	// that we are intentionally keeping accounted for.
	reservations map[string]reservation
}

type reservation struct {
	gpuIndex int
	vramMB   int64
}

func NewLedger() *Ledger {
	return &Ledger{reservations: map[string]reservation{}}
}

// SetGPUs replaces the polled GPU snapshot.
func (l *Ledger) SetGPUs(gpus []*GPUState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gpus = gpus
}

// GPUs returns a copy of the current GPU snapshot.
func (l *Ledger) GPUs() []*GPUState {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*GPUState, 0, len(l.gpus))
	for _, g := range l.gpus {
		cp := *g
		out = append(out, &cp)
	}
	return out
}

// available is the VRAM that can still be admitted on g. Live nvidia-smi
// FreeMB is the physical ceiling; the reservation ledger covers spin-up lag
// so two concurrent loads cannot both see the same free bytes. Fit is the
// tighter of the two.
func (l *Ledger) available(g *GPUState) int64 {
	used := int64(0)
	for _, r := range l.reservations {
		if r.gpuIndex == g.Index {
			used += r.vramMB
		}
	}
	ledger := g.TotalMB - used
	if ledger < 0 {
		ledger = 0
	}
	live := g.FreeMB
	if live < 0 {
		live = 0
	}
	if live < ledger {
		return live
	}
	return ledger
}

// deviceIndex resolves a stanza's device string ("CUDA0", "0", "gpu:0") to a
// GPU index, or -1 for auto.
func deviceIndex(device string) int {
	if device == "" {
		return -1
	}
	d := strings.ToLower(strings.TrimSpace(device))
	d = strings.TrimPrefix(d, "cuda")
	d = strings.TrimPrefix(d, "gpu:")
	var idx int
	if _, err := fmt.Sscanf(d, "%d", &idx); err == nil {
		return idx
	}
	return -1
}

// Reserve attempts to admit a stanza: finds a GPU whose remaining
// (total - reservations) VRAM fits, and records the reservation. Returns the
// chosen GPU index and true on success. Keep the reservation until Unload or
// failed Start.
func (l *Ledger) Reserve(s *config.Stanza) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if idx, exists := l.reservations[s.ModelID]; exists {
		// Already reserved (e.g. concurrent load for the same model).
		return idx.gpuIndex, true
	}
	if len(l.gpus) == 0 {
		// No GPU telemetry yet: allow a single optimistic reservation so the
		// router still works on CPU-only nodes / before first poll. Gate on
		// there being at least one known GPU in practice by requiring vram_mb.
		if s.VramMB <= 0 {
			return -1, true
		}
		return -1, false
	}
	idx, ok := l.fit(s)
	if !ok {
		return -1, false
	}
	l.reservations[s.ModelID] = reservation{gpuIndex: idx, vramMB: s.VramMB}
	return idx, true
}

// CanAdmit reports whether a stanza could be admitted right now, without
// recording a reservation. It is a pure probe for callers (the pool resolver)
// that need to test capacity without disturbing an in-flight reservation a
// concurrent Load may already hold. Reserve is idempotent for an already
// reserved model, so a reserve-then-release probe would drop that reservation.
func (l *Ledger) CanAdmit(s *config.Stanza) bool {
	return l.CanAdmitEvicting(s, nil)
}

// CanAdmitEvicting is CanAdmit as if evict were already unloaded. Live FreeMB
// is credited with those reservations so the probe is not stuck on a stale
// nvidia-smi snapshot from before the unload.
func (l *Ledger) CanAdmitEvicting(s *config.Stanza, evict []string) bool {
	drop := map[string]bool{}
	for _, id := range evict {
		if id != "" {
			drop[id] = true
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.reservations[s.ModelID]; exists {
		return true
	}
	if len(l.gpus) == 0 {
		return s.VramMB <= 0
	}
	want := deviceIndex(s.Device)
	credit := map[int]int64{}
	used := map[int]int64{}
	for id, r := range l.reservations {
		if drop[id] {
			credit[r.gpuIndex] += r.vramMB
			continue
		}
		used[r.gpuIndex] += r.vramMB
	}
	for _, g := range l.gpus {
		if want >= 0 && g.Index != want {
			continue
		}
		ledger := g.TotalMB - used[g.Index]
		if ledger < 0 {
			ledger = 0
		}
		live := g.FreeMB + credit[g.Index]
		if live < 0 {
			live = 0
		}
		if g.TotalMB > 0 && live > g.TotalMB {
			live = g.TotalMB
		}
		avail := ledger
		if live < avail {
			avail = live
		}
		if avail >= s.VramMB {
			return true
		}
		if want >= 0 {
			return false
		}
	}
	return false
}

// fit returns the GPU index a stanza would be admitted to, or false. The
// caller must hold l.mu, must not already hold a reservation for the model,
// and must have at least one GPU in the ledger.
func (l *Ledger) fit(s *config.Stanza) (int, bool) {
	want := deviceIndex(s.Device)
	if want >= 0 {
		for _, g := range l.gpus {
			if g.Index == want {
				if l.available(g) >= s.VramMB {
					return want, true
				}
				return -1, false
			}
		}
		return -1, false // pinned GPU not present
	}

	// auto: pick the GPU with the most available VRAM that fits.
	type cand struct {
		avail int64
		idx   int
	}
	var cands []cand
	for _, g := range l.gpus {
		if l.available(g) >= s.VramMB {
			cands = append(cands, cand{avail: l.available(g), idx: g.Index})
		}
	}
	if len(cands) == 0 {
		return -1, false
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].avail > cands[j].avail })
	return cands[0].idx, true
}

// occupantsOnGPU returns other reserved model ids on gpu, largest first.
// gpu < 0 means every GPU.
func (l *Ledger) occupantsOnGPU(gpu int, except string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	type occ struct {
		id string
		mb int64
	}
	var list []occ
	for id, r := range l.reservations {
		if id == except {
			continue
		}
		if gpu >= 0 && r.gpuIndex != gpu {
			continue
		}
		list = append(list, occ{id: id, mb: r.vramMB})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mb > list[j].mb })
	ids := make([]string, len(list))
	for i, o := range list {
		ids[i] = o.id
	}
	return ids
}

// Release frees a reservation (on unload or failed spin-up).
func (l *Ledger) Release(modelID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.reservations, modelID)
}
