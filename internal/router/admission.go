package router

import (
	"sort"
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
	FreeIfIdleEvictedMB  int64  `json:"free_if_idle_evicted_mb,omitempty"`
	LastSeenUnix         int64  `json:"last_seen_unix"`
}

// Ledger is the sole authority over this node's GPU reservations. Peers only
// ever see our telemetry; they never mutate our ledger. This is what prevents
// split-brain double-booking (SPEC: Admission control).
type Ledger struct {
	mu   sync.Mutex
	gpus []*GPUState
	// reservations maps instance key -> {gpuIndex, vramMB} for backends we
	// have reserved but that may not yet show up in nvidia-smi (spinning up)
	// or that we are intentionally keeping accounted for.
	reservations map[string]reservation
	// pending is VRAM just released whose nvidia-smi FreeMB has not caught
	// up yet. Added to live free until a later poll shows the bytes.
	pending map[int]int64
}

type reservation struct {
	gpuIndex int
	vramMB   int64
	// perGPU is set for multi-GPU stanzas (device: "0,1"). usedOn falls back
	// to vramMB on gpuIndex when perGPU is nil.
	perGPU map[int]int64
}

func (r reservation) usedOn(gpu int) int64 {
	if len(r.perGPU) > 0 {
		return r.perGPU[gpu]
	}
	if r.gpuIndex == gpu {
		return r.vramMB
	}
	return 0
}

func NewLedger() *Ledger {
	return &Ledger{reservations: map[string]reservation{}, pending: map[int]int64{}}
}

// SetGPUs replaces the polled GPU snapshot.
func (l *Ledger) SetGPUs(gpus []*GPUState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	oldFree := map[int]int64{}
	for _, g := range l.gpus {
		oldFree[g.Index] = g.FreeMB
	}
	for _, g := range gpus {
		if old, ok := oldFree[g.Index]; ok {
			if gain := g.FreeMB - old; gain > 0 {
				l.pending[g.Index] -= gain
				if l.pending[g.Index] < 0 {
					l.pending[g.Index] = 0
				}
			}
		}
	}
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

// usageLocked sums reserved MB per GPU. Reservations in drop are counted as
// credit instead, as if already unloaded. Caller holds l.mu.
func (l *Ledger) usageLocked(drop map[string]bool) (used, credit map[int]int64) {
	used = map[int]int64{}
	credit = map[int]int64{}
	for id, r := range l.reservations {
		target := used
		if drop[id] {
			target = credit
		}
		if len(r.perGPU) > 0 {
			for gpu, mb := range r.perGPU {
				target[gpu] += mb
			}
			continue
		}
		target[r.gpuIndex] += r.vramMB
	}
	return used, credit
}

// availLocked is the VRAM that can still be admitted on g. Live nvidia-smi
// FreeMB is the physical ceiling; the reservation ledger covers spin-up lag
// so two concurrent loads cannot both see the same free bytes. Fit is the
// tighter of the two.
func (l *Ledger) availLocked(g *GPUState, used, credit map[int]int64) int64 {
	ledger := g.TotalMB - used[g.Index]
	if ledger < 0 {
		ledger = 0
	}
	live := l.liveFreeLocked(g, credit[g.Index])
	if live < ledger {
		return live
	}
	return ledger
}

// liveFreeLocked is nvidia-smi FreeMB plus pending release credit and any
// extra (evict-probe) credit. Caller holds l.mu.
func (l *Ledger) liveFreeLocked(g *GPUState, extra int64) int64 {
	live := g.FreeMB + l.pending[g.Index] + extra
	if live < 0 {
		live = 0
	}
	if g.TotalMB > 0 && live > g.TotalMB {
		live = g.TotalMB
	}
	return live
}

// deviceIndex resolves a stanza's device string ("CUDA0", "0", "gpu:0") to a
// GPU index, or -1 for auto. A list ("0,1" or "1|2") returns the first index.
func deviceIndex(device string) int {
	idxs := deviceIndexes(device)
	if len(idxs) == 0 {
		return -1
	}
	return idxs[0]
}

// deviceIndexes returns every GPU a device string names. Empty means auto.
func deviceIndexes(device string) []int {
	idxs, _, _ := config.ParseDevice(device)
	return idxs
}

// splitPlacement is true when the stanza spans every GPU its device lists.
func splitPlacement(s *config.Stanza) bool {
	_, split, _ := config.ParseDevice(s.Device)
	return split
}

func (l *Ledger) gpuLocked(idx int) *GPUState {
	for _, g := range l.gpus {
		if g.Index == idx {
			return g
		}
	}
	return nil
}

// candidatesLocked lists the GPUs a single-GPU stanza may use, in the order
// its device field names them (auto: nvidia-smi order). Caller holds l.mu.
func (l *Ledger) candidatesLocked(s *config.Stanza) []*GPUState {
	idxs := deviceIndexes(s.Device)
	if len(idxs) == 0 {
		return l.gpus
	}
	var out []*GPUState
	for _, idx := range idxs {
		if g := l.gpuLocked(idx); g != nil {
			out = append(out, g)
		}
	}
	return out
}

// CandidateGPUs is candidatesLocked for callers outside the ledger.
func (l *Ledger) CandidateGPUs(s *config.Stanza) []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []int
	for _, g := range l.candidatesLocked(s) {
		out = append(out, g.Index)
	}
	return out
}

// fitBucketMB rounds leftover VRAM when ranking GPUs so nvidia-smi noise
// between two identical empty cards does not override the device order.
const fitBucketMB int64 = 1024

// rankLocked returns the candidate GPUs that fit s, best first. only >= 0
// restricts the answer to that GPU. Ranking: a GPU not already hosting
// another instance of the same model, then the tightest fit (keeps whole
// cards free for large models), then device order. Caller holds l.mu.
func (l *Ledger) rankLocked(key string, s *config.Stanza, used, credit map[int]int64, only int) []int {
	type cand struct {
		idx     int
		order   int
		sibling bool
		left    int64
	}
	var cands []cand
	for order, g := range l.candidatesLocked(s) {
		if only >= 0 && g.Index != only {
			continue
		}
		avail := l.availLocked(g, used, credit)
		if avail < s.VramMB {
			continue
		}
		cands = append(cands, cand{
			idx:     g.Index,
			order:   order,
			sibling: l.hostsSiblingLocked(key, s.ModelID, g.Index),
			left:    (avail - s.VramMB) / fitBucketMB,
		})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.sibling != b.sibling {
			return !a.sibling
		}
		if a.left != b.left {
			return a.left < b.left
		}
		return a.order < b.order
	})
	out := make([]int, len(cands))
	for i, c := range cands {
		out[i] = c.idx
	}
	return out
}

// hostsSiblingLocked reports whether gpu already holds another instance of
// modelID. Caller holds l.mu.
func (l *Ledger) hostsSiblingLocked(key, modelID string, gpu int) bool {
	for id, r := range l.reservations {
		if id != key && instanceModel(id) == modelID && r.usedOn(gpu) > 0 {
			return true
		}
	}
	return false
}

// splitVRAM shares total MB across pinned GPUs in proportion to caps.
// caps are available bytes (free VRAM), not card size, so a resident on a
// smaller card keeps its reservation and the emptier cards take more of the
// model. The last GPU absorbs rounding so the shares sum to total.
func splitVRAM(total int64, idxs []int, caps []int64) map[int]int64 {
	shares := make(map[int]int64, len(idxs))
	if len(idxs) == 0 {
		return shares
	}
	var capSum int64
	for _, c := range caps {
		if c > 0 {
			capSum += c
		}
	}
	if capSum <= 0 || total <= 0 {
		each := total / int64(len(idxs))
		var assigned int64
		for i, idx := range idxs {
			if i == len(idxs)-1 {
				shares[idx] = total - assigned
				break
			}
			shares[idx] = each
			assigned += each
		}
		return shares
	}
	var assigned int64
	for i, idx := range idxs {
		if i == len(idxs)-1 {
			shares[idx] = total - assigned
			break
		}
		c := int64(0)
		if i < len(caps) && caps[i] > 0 {
			c = caps[i]
		}
		n := total * c / capSum
		shares[idx] = n
		assigned += n
	}
	return shares
}

// splitFitLocked shares s across every GPU its device lists, or false if any
// share does not fit. Caller holds l.mu.
func (l *Ledger) splitFitLocked(s *config.Stanza, used, credit map[int]int64) (map[int]int64, bool) {
	idxs := deviceIndexes(s.Device)
	caps := make([]int64, len(idxs))
	for i, idx := range idxs {
		g := l.gpuLocked(idx)
		if g == nil {
			return nil, false
		}
		caps[i] = l.availLocked(g, used, credit)
	}
	shares := splitVRAM(s.VramMB, idxs, caps)
	for i, idx := range idxs {
		if caps[i] < shares[idx] {
			return nil, false
		}
	}
	return shares, true
}

// Reserve attempts to admit instance key of stanza s: finds a GPU whose
// remaining VRAM fits and records the reservation. Returns the chosen GPU
// index and true on success. Keep the reservation until Unload or failed
// Start.
func (l *Ledger) Reserve(key string, s *config.Stanza) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, exists := l.reservations[key]; exists {
		// Already reserved (e.g. concurrent load for the same instance).
		return r.gpuIndex, true
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
	used, credit := l.usageLocked(nil)
	if splitPlacement(s) {
		shares, ok := l.splitFitLocked(s, used, credit)
		if !ok {
			return -1, false
		}
		idx := deviceIndex(s.Device)
		l.reservations[key] = reservation{gpuIndex: idx, vramMB: s.VramMB, perGPU: shares}
		return idx, true
	}
	ranked := l.rankLocked(key, s, used, credit, -1)
	if len(ranked) == 0 {
		return -1, false
	}
	l.reservations[key] = reservation{gpuIndex: ranked[0], vramMB: s.VramMB}
	return ranked[0], true
}

// CanAdmit reports whether instance key could be admitted right now, without
// recording a reservation. It is a pure probe for callers that need to test
// capacity without disturbing an in-flight reservation a concurrent Load may
// already hold. Reserve is idempotent for an already reserved instance, so a
// reserve-then-release probe would drop that reservation.
func (l *Ledger) CanAdmit(key string, s *config.Stanza) bool {
	return l.CanAdmitEvicting(key, s, nil)
}

// CanAdmitEvicting is CanAdmit as if evict were already unloaded. Live FreeMB
// is credited with those reservations so the probe is not stuck on a stale
// nvidia-smi snapshot from before the unload.
func (l *Ledger) CanAdmitEvicting(key string, s *config.Stanza, evict []string) bool {
	return l.canAdmitOn(key, s, -1, evict)
}

// CanAdmitOn is CanAdmitEvicting restricted to one GPU (single-GPU stanzas).
func (l *Ledger) CanAdmitOn(key string, s *config.Stanza, gpu int, evict []string) bool {
	return l.canAdmitOn(key, s, gpu, evict)
}

func (l *Ledger) canAdmitOn(key string, s *config.Stanza, only int, evict []string) bool {
	drop := map[string]bool{}
	for _, id := range evict {
		if id != "" {
			drop[id] = true
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.reservations[key]; exists {
		return true
	}
	if len(l.gpus) == 0 {
		return s.VramMB <= 0
	}
	used, credit := l.usageLocked(drop)
	if splitPlacement(s) {
		_, ok := l.splitFitLocked(s, used, credit)
		return ok
	}
	return len(l.rankLocked(key, s, used, credit, only)) > 0
}

// occupantsOnGPU returns other reserved instance keys on gpu, largest first.
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
		if gpu >= 0 && r.usedOn(gpu) == 0 {
			continue
		}
		mb := r.vramMB
		if gpu >= 0 && r.usedOn(gpu) > 0 {
			mb = r.usedOn(gpu)
		}
		list = append(list, occ{id: id, mb: mb})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].mb != list[j].mb {
			return list[i].mb > list[j].mb
		}
		return list[i].id < list[j].id
	})
	ids := make([]string, len(list))
	for i, o := range list {
		ids[i] = o.id
	}
	return ids
}

// reservedGPU is the GPU an instance is booked on, or -1.
func (l *Ledger) reservedGPU(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.reservations[key]
	if !ok {
		return -1
	}
	return r.gpuIndex
}

// reservationUsed copies per-GPU VRAM this instance holds.
func (l *Ledger) reservationUsed(key string) map[int]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.reservations[key]
	if !ok {
		return nil
	}
	out := map[int]int64{}
	if len(r.perGPU) > 0 {
		for gpu, mb := range r.perGPU {
			if mb > 0 {
				out[gpu] = mb
			}
		}
		return out
	}
	if r.gpuIndex >= 0 && r.vramMB > 0 {
		out[r.gpuIndex] = r.vramMB
	}
	return out
}

// Release frees a reservation (on unload or failed spin-up).
func (l *Ledger) Release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.reservations, key)
}

// Credit records VRAM that Unload just freed, before nvidia-smi catches up.
func (l *Ledger) Credit(gpu int, mb int64) {
	if gpu < 0 || mb <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending[gpu] += mb
}
