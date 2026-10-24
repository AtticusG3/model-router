package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"model-router/internal/config"
)

// Router is one node's full instance of the model router. It owns the local
// backends, the admission ledger, and the peer cache.
type Router struct {
	cfg    *config.Config
	logger *Logger
	ledger *Ledger
	peers  *PeerCache
	node   string

	mu        sync.Mutex
	managed   map[string]*Managed // local stanza id -> supervisor
	occupancy map[string]int      // target key -> in-flight selected requests
	draining  map[string]bool     // claimed for unload; new requests wait
	spawn     spawner             // tests inject a fake; nil uses the GOOS default
	admitWait chan struct{}       // 1-buffer poke when occupancy/unload may free VRAM
	shutdown  chan struct{}
	shutOnce  sync.Once

	httpc    *http.Client
	activity *ActivityLog
}

// New builds a Router from parsed config.
func New(cfg *config.Config, logger *Logger, node string) *Router {
	stale := time.Duration(cfg.Telemetry.PeerStaleSeconds) * time.Second
	return &Router{
		cfg:       cfg,
		logger:    logger,
		ledger:    NewLedger(),
		peers:     NewPeerCache(stale),
		node:      node,
		managed:   map[string]*Managed{},
		occupancy: map[string]int{},
		draining:  map[string]bool{},
		admitWait: make(chan struct{}, 1),
		shutdown:  make(chan struct{}),
		httpc:     &http.Client{Timeout: 0}, // no overall timeout; streaming is long
		activity:  NewActivityLog(),
	}
}

// Shutdown tells supervisors not to restart backends and cancels in-flight
// peer loads. Call it before unloading on process exit.
func (r *Router) Shutdown() {
	r.shutOnce.Do(func() { close(r.shutdown) })
}

// Activity returns the in-memory generation request log for the operator UI.
func (r *Router) Activity() *ActivityLog { return r.activity }

// Ledger returns the admission ledger (used by the telemetry poller).
func (r *Router) Ledger() *Ledger { return r.ledger }

// Peers returns the peer cache (used by the peer syncer and status).
func (r *Router) Peers() *PeerCache { return r.peers }

// Load ensures the given local stanza (or "model#n" instance) is running.
// Fail-fast: preload and tests use this. Request paths use load(..., true)
// so a busy GPU queues instead of 503.
func (r *Router) Load(modelID string) (int, error) {
	return r.load(context.Background(), modelID, false)
}

func (r *Router) load(ctx context.Context, key string, wait bool) (int, error) {
	s := r.cfg.Stanza(instanceModel(key))
	if n := instanceIndex(key); s == nil || n < 0 || n >= s.InstanceCount() {
		return -1, fmt.Errorf("unknown local model %q", key)
	}
	// An extra copy is opportunistic: if its GPU is gone, the caller queues
	// on a sibling instead of evicting or waiting here.
	evictOK := !r.hasLiveSibling(key)
	if !evictOK {
		wait = false
	}

	loggedWait := false
	hardRetries := 0
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return -1, lastErr
			}
			return -1, err
		}
		r.mu.Lock()
		m := r.managedLocked(key, s)
		r.mu.Unlock()

		if m.State() == StateRunning {
			return m.gpuIndex(), nil
		}

		// Admission control: reserve VRAM before spawning. Keep it until Unload
		// or failed Start so crash-restart does not need to re-reserve.
		// A hard start failure (the child died) gets one quiet retry so a
		// stale nvidia-smi reading can catch up. That retry must not evict
		// neighbors: the failure was this model, not a busy card.
		gpu, reserved := r.ledger.Reserve(key, s)
		if !reserved && hardRetries == 0 && evictOK {
			r.evictToFit(key, s)
			gpu, reserved = r.ledger.Reserve(key, s)
		}
		if reserved {
			m.place(gpu)
			if gpu >= 0 {
				r.logger.Infof("%s placed on GPU %d", key, gpu)
			}
			_, err := m.Start()
			if err != nil {
				var joined joinedStart
				if errors.As(err, &joined) {
					return -1, joined.err
				}
				lastErr = err
				r.ledger.Release(key)
				r.forgetManaged(key, m)
				if !wait || !r.canEventuallyAdmit(s) {
					return -1, err
				}
				if hardStartFailure(err) {
					if hardRetries >= 1 {
						return -1, err
					}
					hardRetries++
					r.logger.Infof("retrying load of %s after %v", key, err)
					if err := r.waitAdmit(ctx); err != nil {
						return -1, lastErr
					}
					continue
				}
				if !stoppedDuringStart(err) {
					return -1, err
				}
				r.logger.Infof("retrying load of %s after %v", key, err)
				if err := r.waitAdmit(ctx); err != nil {
					return -1, lastErr
				}
				continue
			}
			m.SetGPU(gpu)
			return gpu, nil
		}
		if hardRetries > 0 || !wait || !r.canEventuallyAdmit(s) {
			if !evictOK {
				r.forgetManaged(key, m)
			}
			if lastErr != nil {
				return -1, lastErr
			}
			return -1, fmt.Errorf("no GPU has %d MB free for %s", s.VramMB, key)
		}
		if !loggedWait {
			r.logger.Infof("waiting for %d MB for %s (GPU busy)", s.VramMB, key)
			loggedWait = true
		}
		if err := r.waitAdmit(ctx); err != nil {
			return -1, err
		}
	}
}

func stanzaGPUs(s *config.Stanza) []int {
	idxs := deviceIndexes(s.Device)
	if len(idxs) == 0 {
		return []int{deviceIndex(s.Device)}
	}
	return idxs
}

// canEventuallyAdmit is true unless we know the pinned (or every) card is
// smaller than s. Missing telemetry and a missing pinned GPU wait rather
// than 503: occupied VRAM can free after idle eviction.
func (r *Router) canEventuallyAdmit(s *config.Stanza) bool {
	idxs := deviceIndexes(s.Device)
	gpus := r.ledger.GPUs()
	if len(gpus) == 0 {
		return true
	}
	totals := map[int]int64{}
	for _, g := range gpus {
		totals[g.Index] = g.TotalMB
	}
	if splitPlacement(s) {
		var capSum int64
		for _, idx := range idxs {
			t := totals[idx]
			if t <= 0 {
				return true // GPU not in telemetry yet
			}
			capSum += t
		}
		var assigned int64
		for i, idx := range idxs {
			var need int64
			if i == len(idxs)-1 {
				need = s.VramMB - assigned
			} else {
				need = s.VramMB * totals[idx] / capSum
				assigned += need
			}
			if totals[idx] < need {
				return false
			}
		}
		return true
	}
	pinned := len(idxs) > 0
	if !pinned {
		for _, g := range gpus {
			idxs = append(idxs, g.Index)
		}
	}
	saw := false
	for _, idx := range idxs {
		total, ok := totals[idx]
		if !ok {
			continue
		}
		saw = true
		if total <= 0 || total >= s.VramMB {
			return true
		}
	}
	return pinned && !saw
}

func stoppedDuringStart(err error) bool {
	return err != nil && strings.Contains(err.Error(), "stopped during start")
}

// hardStartFailure means the child died or never became healthy. Retrying it
// by evicting other models does not make a too-large KV cache fit.
func hardStartFailure(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "exited during start") ||
		strings.Contains(s, "health check timed out") ||
		strings.Contains(s, "process not alive")
}

func (r *Router) forgetManaged(modelID string, m *Managed) {
	r.mu.Lock()
	if r.managed[modelID] == m {
		delete(r.managed, modelID)
	}
	r.mu.Unlock()
	if m != nil {
		m.Stop()
	}
}

func (r *Router) pokeAdmit() {
	select {
	case r.admitWait <- struct{}{}:
	default:
	}
}

func (r *Router) waitAdmit(ctx context.Context) error {
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.admitWait:
		return nil
	case <-timer.C:
		return nil
	}
}

// Unload stops a local model: every instance for a model id, or a single one
// for "model#n".
func (r *Router) Unload(id string) error {
	s := r.cfg.Stanza(id)
	if s == nil || s.InstanceCount() == 1 {
		return r.unloadInstance(id)
	}
	unloaded := false
	for n := 0; n < s.InstanceCount(); n++ {
		if r.unloadInstance(instanceKey(id, n)) == nil {
			unloaded = true
		}
	}
	if !unloaded {
		return fmt.Errorf("not loaded: %s", id)
	}
	return nil
}

// unloadInstance stops one backend and releases its VRAM reservation. A
// ledger reservation without a process still gets released so eviction
// cannot 503 on a ghost occupant.
func (r *Router) unloadInstance(key string) error {
	defer func() {
		r.mu.Lock()
		delete(r.draining, key)
		r.mu.Unlock()
	}()
	r.mu.Lock()
	m, ok := r.managed[key]
	delete(r.managed, key)
	r.mu.Unlock()
	gpu := r.ledger.reservedGPU(key)
	used := r.ledger.reservationUsed(key)
	if m != nil {
		if g := m.gpuIndex(); g >= 0 {
			gpu = g
		}
		m.Stop()
	}
	vram := r.modelVram(key)
	had := gpu >= 0 || ok || len(used) > 0
	r.ledger.Release(key)
	if len(used) > 0 {
		for g, mb := range used {
			r.ledger.Credit(g, mb)
		}
	} else {
		r.ledger.Credit(gpu, vram)
	}
	r.pokeAdmit()
	if !had {
		return fmt.Errorf("not loaded: %s", key)
	}
	return nil
}

// SweepStale actively evicts running models whose idle TTL has expired.
// Lazy eviction (evictToFit) only reclaims VRAM when a new load needs it;
// this sweep makes ttl a real eviction deadline so VRAM returns to the
// system (e.g. for hashcat's governor to see) even with no incoming load.
// Models with ttl <= 0 are resident and never swept. In-flight requests,
// starting/stopping processes are protected. Each instance ages on its own,
// so an idle extra copy is reclaimed while a busy sibling stays.
func (r *Router) SweepStale() {
	r.mu.Lock()
	keys := make([]string, 0, len(r.managed))
	for key := range r.managed {
		keys = append(keys, key)
	}
	r.mu.Unlock()
	sort.Strings(keys)
	for _, key := range keys {
		if !r.claimForSweep(key) {
			continue
		}
		r.logger.Infof("idle TTL expired: evicting %s", key)
		if err := r.unloadInstance(key); err != nil {
			r.logger.Errorf("ttl evict %s: %v", key, err)
		}
	}
}

// claimForSweep marks a stale idle model so a request cannot take a slot
// between the check and Unload. Unload clears the mark.
func (r *Router) claimForSweep(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining[id] || r.occupancy[id] > 0 {
		return false
	}
	m := r.managed[id]
	if m == nil || !m.IsStale(time.Now()) {
		return false
	}
	r.draining[id] = true
	return true
}

// IdleTTLReaper runs SweepStale on a ticker until ctx is cancelled.
func IdleTTLReaper(ctx context.Context, r *Router, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.SweepStale()
		}
	}
}

// evictKind selects which occupants one eviction pass may unload.
type evictKind int

const (
	evictStale evictKind = iota
	evictIdle
	evictResident
)

var evictKinds = []evictKind{evictStale, evictIdle, evictResident}

func (k evictKind) String() string {
	switch k {
	case evictStale:
		return "stale"
	case evictIdle:
		return "idle"
	case evictResident:
		return "resident"
	default:
		return fmt.Sprintf("evictKind(%d)", int(k))
	}
}

// evictToFit unloads occupants until the ledger can admit instance key of s.
// Stale non-residents go first, then any other idle non-resident, then ttl=0
// residents (the 3090 intent/embed/rerank trio) only as a last resort.
func (r *Router) evictToFit(key string, s *config.Stanza) {
	if splitPlacement(s) {
		r.evictSplit(key, s)
		return
	}
	r.evictOneGPU(key, s)
}

// evictSplit frees room for a model that spans several GPUs: it needs a share
// on every listed card, so each pass walks all of them.
func (r *Router) evictSplit(key string, s *config.Stanza) {
	for _, kind := range evictKinds {
		for _, gpu := range stanzaGPUs(s) {
			for _, id := range r.ledger.occupantsOnGPU(gpu, key) {
				if !r.claimForEvict(id, kind) {
					continue
				}
				r.evict(id, kind, key, gpu)
				if r.ledger.CanAdmit(key, s) {
					return
				}
			}
		}
	}
}

// evictOneGPU frees a single card for a single-GPU model. Every candidate GPU
// is planned first and only the gentlest plan runs: the lowest eviction kind
// that works (stale before idle before resident), then the fewest MB
// unloaded, then device order. So a stale model on one V100 is evicted
// before an idle one on the other, and a card is never half-emptied for
// nothing while the model lands somewhere else.
func (r *Router) evictOneGPU(key string, s *config.Stanza) {
	if r.ledger.CanAdmit(key, s) {
		return
	}
	type victim struct {
		id   string
		kind evictKind
	}
	for level, upTo := range evictKinds {
		var best []victim
		bestGPU, bestMB := -1, int64(-1)
		for _, gpu := range r.ledger.CandidateGPUs(s) {
			var plan []victim
			var ids []string
			var mb int64
			picked := map[string]bool{}
			fits := false
		fill:
			for _, kind := range evictKinds[:level+1] {
				for _, id := range r.ledger.occupantsOnGPU(gpu, key) {
					if picked[id] || !r.evictable(id, kind) {
						continue
					}
					picked[id] = true
					plan = append(plan, victim{id, kind})
					ids = append(ids, id)
					mb += r.ledger.reservationUsed(id)[gpu]
					if fits = r.ledger.CanAdmitOn(key, s, gpu, ids); fits {
						break fill
					}
				}
			}
			if fits && (bestMB < 0 || mb < bestMB) {
				best, bestGPU, bestMB = plan, gpu, mb
			}
		}
		if bestGPU < 0 {
			continue
		}
		r.logger.Infof("freeing GPU %d for %s (%s eviction, %d MB)", bestGPU, key, upTo, bestMB)
		for _, v := range best {
			if !r.claimForEvict(v.id, v.kind) {
				return // raced with a request; the load loop re-plans
			}
			r.evict(v.id, v.kind, key, bestGPU)
		}
		return
	}
}

func (r *Router) evict(id string, kind evictKind, forKey string, gpu int) {
	r.logger.Infof("evicting %s %s from GPU %d to admit %s", kind, id, gpu, forKey)
	if err := r.unloadInstance(id); err != nil {
		r.logger.Errorf("evict %s: %v", id, err)
	}
}

// claimForEvict marks an occupant for unload if kind allows it. Unload
// clears the mark.
func (r *Router) claimForEvict(id string, kind evictKind) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.evictableLocked(id, kind) {
		return false
	}
	r.draining[id] = true
	return true
}

func (r *Router) evictable(id string, kind evictKind) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.evictableLocked(id, kind)
}

// evictableLocked reports whether kind allows unloading id now. vram_mb <= 0
// does not free a GPU, so those stanzas are left alone. Caller holds r.mu.
func (r *Router) evictableLocked(id string, kind evictKind) bool {
	if r.draining[id] || r.occupancy[id] > 0 {
		return false
	}
	if r.modelVram(id) <= 0 {
		return false
	}
	m := r.managed[id]
	sticky := r.residentModel(id)
	switch kind {
	case evictStale:
		if sticky || m == nil || !m.IsStale(time.Now()) {
			return false
		}
	case evictIdle:
		if sticky {
			return false
		}
	case evictResident:
		if !sticky {
			return false
		}
	default:
		return false
	}
	if m != nil {
		// A backend inside its hold window just became healthy; evicting it
		// would discard a long warm-up before any client could reach it.
		// Checked here rather than via protected() because the caller already
		// holds r.mu and protected() re-locks it. m.InHold takes m.mu only.
		if m.InHold(time.Now()) {
			return false
		}
		switch m.State() {
		case StateStarting, StateStopping:
			return false
		}
	}
	return true
}

// residentModel is a ttl=0 stanza. Those stay loaded across sweeps and are
// the last models evictToFit will touch.
func (r *Router) residentModel(id string) bool {
	s := r.stanzaOf(id)
	return s != nil && s.IdleTTLSeconds <= 0
}

// protected is true while a request is mid-generation, the process is still
// spinning up or down, or the stanza's hold window is open. Evicting a start
// races two loads into "stopped during start" 503s; wait instead. A held model
// just became healthy, so evicting it here would discard a long warm-up (cold
// load, one-off calibration) before any client could use it.
func (r *Router) protected(id string) bool {
	r.mu.Lock()
	busy := r.occupancy[id] > 0
	m := r.managed[id]
	r.mu.Unlock()
	if busy {
		return true
	}
	if m == nil {
		return false
	}
	if m.InHold(time.Now()) {
		return true
	}
	switch m.State() {
	case StateStarting, StateStopping:
		return true
	}
	return false
}

// Target is a concrete serving destination after pool/spillover resolution.
type Target struct {
	Local  string
	Peer   string
	PeerID string
}

// resolveTarget resolves a ModelRef to a concrete serving target:
//   - local stanza id (may need to be spawned)
//   - pool name (spillover resolution, compat for existing clients)
//   - peer-qualified "peer/model"
//   - mesh id advertised by a peer (first reachable node that lists it)
func (r *Router) resolveTarget(ref ModelRef) (Target, error) {
	switch {
	case ref.Local != "":
		return Target{Local: ref.Local}, nil
	case ref.Pool != "":
		return r.resolvePool(ref.Pool)
	case ref.Peer != "":
		p := r.cfg.Peer(ref.Peer)
		if p == nil {
			return Target{}, fmt.Errorf("unknown peer %q", ref.Peer)
		}
		if !r.peerHasModel(p, ref.PeerID) {
			return Target{}, fmt.Errorf("peer %s does not serve %q", ref.Peer, ref.PeerID)
		}
		return Target{Peer: ref.Peer, PeerID: ref.PeerID}, nil
	case ref.Raw != "":
		return r.resolveMesh(ref.Raw)
	}
	return Target{}, ErrNoModel
}

// resolveMesh picks a peer that advertises modelID. Local stanzas are
// resolved before this runs. Prefers a peer that already has a free slot,
// then one that can admit a fresh load. A loaded-but-full peer is last
// resort (that node will queue).
func (r *Router) resolveMesh(modelID string) (Target, error) {
	return r.pickPeerForModel(modelID, false)
}

// pickPeerForModel ranks router peers that advertise modelID.
// slotSpill skips loaded-but-full peers so the caller can queue locally.
func (r *Router) pickPeerForModel(modelID string, slotSpill bool) (Target, error) {
	var admit, wait, full Target
	haveAdmit, haveWait, haveFull := false, false, false
	for i := range r.cfg.Peers {
		p := &r.cfg.Peers[i]
		if !r.peerHasModel(p, modelID) {
			continue
		}
		if p.Kind == "openai" {
			return Target{Peer: p.Name, PeerID: modelID}, nil
		}
		if !r.peers.Fresh(p.Name) {
			continue
		}
		loaded, free, known := r.peerSlotInfo(p.Name, modelID)
		if known && loaded && free {
			return Target{Peer: p.Name, PeerID: modelID}, nil
		}
		if known && loaded && !free {
			if !slotSpill && !haveFull {
				full = Target{Peer: p.Name, PeerID: modelID}
				haveFull = true
			}
			continue
		}
		if r.peerEligible(p.Name, modelID) {
			if !haveAdmit {
				admit = Target{Peer: p.Name, PeerID: modelID}
				haveAdmit = true
			}
			continue
		}
		// Card is occupied (in-flight, or our local vram_mb is larger than
		// their free bytes). They listed this model_id — POST /_router/load
		// and let that node wait/evict instead of 503 here.
		if !haveWait {
			wait = Target{Peer: p.Name, PeerID: modelID}
			haveWait = true
		}
	}
	if haveAdmit {
		return admit, nil
	}
	if haveWait {
		return wait, nil
	}
	if haveFull {
		return full, nil
	}
	return Target{}, fmt.Errorf("no node can serve %q", modelID)
}

func (r *Router) peerHasModel(p *config.Peer, modelID string) bool {
	for _, m := range p.Models {
		if m == modelID {
			return true
		}
	}
	return false
}

func occupancyKey(t Target) string {
	if t.Local != "" {
		return t.Local
	}
	if t.Peer != "" {
		return t.Peer + "/" + t.PeerID
	}
	return ""
}

func (r *Router) releaseOccupancy(t Target) {
	key := occupancyKey(t)
	if key == "" {
		return
	}
	r.mu.Lock()
	if r.occupancy[key] > 0 {
		r.occupancy[key]--
	}
	r.mu.Unlock()
	r.pokeAdmit()
}

func (r *Router) busy(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.occupancy[id] > 0
}

func (r *Router) holdOccupancy(id string) {
	for {
		r.mu.Lock()
		if !r.draining[id] {
			r.occupancy[id]++
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		select {
		case <-r.shutdown:
			r.mu.Lock()
			r.occupancy[id]++
			r.mu.Unlock()
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (r *Router) occupancyOf(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.occupancy[id]
}

// localCanServe reports whether s is up (or starting) here, or could be
// admitted by evicting occupants that are not mid-request.
func (r *Router) localCanServe(s *config.Stanza) bool {
	if s == nil {
		return false
	}
	r.mu.Lock()
	live := len(r.liveInstancesLocked(s)) > 0
	r.mu.Unlock()
	if live {
		return true
	}
	key := instanceKey(s.ModelID, 0)
	var evict []string
	blocked := false
	seen := map[string]bool{}
	for _, gpu := range stanzaGPUs(s) {
		for _, id := range r.ledger.occupantsOnGPU(gpu, key) {
			if seen[id] {
				continue
			}
			seen[id] = true
			if r.protected(id) {
				blocked = true
				continue
			}
			evict = append(evict, id)
		}
	}
	if r.ledger.CanAdmitEvicting(key, s, evict) {
		return true
	}
	// nvidia-smi free can stay below vram_mb after idle eviction until the
	// process dies. If nothing is generating, wait/evict locally instead of 503.
	return !blocked && r.canEventuallyAdmit(s)
}

// peerEligible reports whether a peer is a spillover candidate. A peer that
// already has the model loaded is eligible when it has a free slot (VRAM is
// already spent). A peer that does not have it loaded uses cached VRAM as a
// hint. Unknown VRAM still tries a fresh peer and lets that node admit.
func (r *Router) peerEligible(name, modelID string) bool {
	p := r.cfg.Peer(name)
	if p == nil {
		return false
	}
	if p.Kind == "openai" {
		return true
	}
	if !r.peers.Fresh(name) {
		return false
	}
	loaded, free, known := r.peerSlotInfo(name, modelID)
	if known && loaded {
		return free
	}
	vram := r.modelVram(modelID)
	if vram > 0 && !r.peerFits(name, vram) {
		return false
	}
	return true
}

func (r *Router) peerSlotInfo(name, modelID string) (loaded, free, known bool) {
	t := r.peers.Snapshot()[name]
	if t == nil {
		return false, false, false
	}
	// A peer lists each instance of a model separately; any free one counts.
	for _, m := range t.LoadedModels {
		if m.ID != modelID {
			continue
		}
		loaded = true
		slots := m.Slots
		if slots <= 0 {
			slots = 1
		}
		if m.InFlight < slots {
			free = true
		}
	}
	return loaded, free, true
}

func (r *Router) modelVram(id string) int64 {
	if s := r.stanzaOf(id); s != nil {
		return s.VramMB
	}
	return 0
}

// peerEmptyHeadroomMB is how close idle-evicted free must be to the card
// total before we treat the GPU as empty enough to run that node's own
// stanza. nvidia-smi never reports 100% free (driver crumbs).
const peerEmptyHeadroomMB int64 = 2048

// peerFits reports whether cached peer telemetry shows a GPU that can take
// this model now, or after that peer evicts idle (not in-flight) occupants.
// Used only to pick a candidate; the peer admits for real. Local vram_mb is
// a hint, not a clamp-to-total: a 24GB node that lists the model still
// fits when its card is empty even if our stanza is 27GB.
func (r *Router) peerFits(name string, vramMB int64) bool {
	if vramMB <= 0 {
		return false
	}
	if !r.peers.Fresh(name) {
		return false
	}
	t := r.peers.Snapshot()[name]
	if t == nil {
		return false
	}
	for _, g := range t.GPUs {
		evictable := g.FreeIfIdleEvictedMB
		if evictable < g.FreeIfStaleEvictedMB {
			evictable = g.FreeIfStaleEvictedMB
		}
		if evictable < g.FreeMB {
			evictable = g.FreeMB
		}
		if evictable >= vramMB {
			return true
		}
		if g.TotalMB > 0 && evictable+peerEmptyHeadroomMB >= g.TotalMB {
			return true
		}
	}
	return false
}

// resolvePool implements spillover selection. Targets are tried in order; a
// local target is used when occupancy is below the spillover cap and it is
// running or can start (including by evicting idle neighbors). A peer is used
// when occupancy is below the cap and the peer is eligible. Occupancy
// increments when a target is chosen.
func (r *Router) resolvePool(poolName string) (Target, error) {
	pool := r.cfg.Pool(poolName)
	if pool == nil {
		return Target{}, fmt.Errorf("unknown pool %q", poolName)
	}
	t, err := r.pickPoolTarget(pool, nil)
	if err != nil {
		return Target{}, err
	}
	return t, nil
}

func (r *Router) pickPoolTarget(pool *config.Pool, skip map[string]bool) (Target, error) {
	for _, target := range pool.Targets {
		if skip[target] {
			continue
		}
		ref := resolveRef(target, r.cfg)
		var cand Target
		switch {
		case ref.Local != "":
			cand = Target{Local: ref.Local}
		case ref.Peer != "":
			cand = Target{Peer: ref.Peer, PeerID: ref.PeerID}
		default:
			continue
		}
		if skip[occupancyKey(cand)] {
			continue
		}
		limit := pool.Spillover
		if limit <= 0 {
			if ref.Local != "" {
				limit = r.cfg.Stanza(ref.Local).SlotCount()
			} else {
				limit = 1
			}
		}
		switch {
		case ref.Local != "":
			t := cand
			key := occupancyKey(t)
			r.mu.Lock()
			busy := r.occupancy[key]
			m, ok := r.managed[ref.Local]
			running := ok && m.State() == StateRunning
			if busy >= limit {
				r.mu.Unlock()
				continue
			}
			if running {
				r.occupancy[key]++
				r.mu.Unlock()
				return t, nil
			}
			r.mu.Unlock()
			if r.localCanServe(r.cfg.Stanza(ref.Local)) {
				r.mu.Lock()
				if r.occupancy[key] >= limit {
					r.mu.Unlock()
					continue
				}
				r.occupancy[key]++
				r.mu.Unlock()
				return t, nil
			}
		case ref.Peer != "":
			t := Target{Peer: ref.Peer, PeerID: ref.PeerID}
			key := occupancyKey(t)
			r.mu.Lock()
			busy := r.occupancy[key]
			if busy >= limit {
				r.mu.Unlock()
				continue
			}
			r.mu.Unlock()
			if r.peerEligible(ref.Peer, ref.PeerID) {
				r.mu.Lock()
				if r.occupancy[key] >= limit {
					r.mu.Unlock()
					continue
				}
				r.occupancy[key]++
				r.mu.Unlock()
				return t, nil
			}
		}
	}
	return Target{}, fmt.Errorf("no capacity for pool %q (all targets busy/unreachable)", pool.Name)
}

// ServeHTTP is the entry for model-routed endpoints.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ref, err := matchRequest(req, r.cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if ref.Pool != "" {
		r.servePool(w, req, ref.Pool)
		return
	}
	tgt, err := r.resolveTarget(ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	switch {
	case tgt.Local != "":
		r.serveLocalTarget(w, req, tgt.Local)
	case tgt.Peer != "":
		if err := r.proxyPeer(w, req, tgt.Peer, tgt.PeerID); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		}
	default:
		http.Error(w, "no router for requested model", http.StatusNotFound)
	}
}

func (r *Router) serveLocalTarget(w http.ResponseWriter, req *http.Request, modelID string) {
	incomingPeer := req.Header.Get("X-Model-Router-Peer") != ""
	// If this GPU cannot admit now, try an idle peer before waiting locally.
	if !incomingPeer {
		if s := r.cfg.Stanza(modelID); s != nil && !r.localCanServe(s) {
			if r.serveSpill(w, req, modelID) {
				return
			}
		}
	}
	// A free slot on a running instance, or a new instance on a free GPU,
	// beats sending the request off-node.
	if s := r.cfg.Stanza(modelID); !incomingPeer && s != nil {
		if key, ok := r.acquireInstance(s, false); ok {
			err := r.proxyLocalHeld(w, req, key)
			r.releaseOccupancy(Target{Local: key})
			if err == nil {
				return
			}
			if r.hasLiveSibling(key) && r.proxyLocal(w, req, modelID) == nil {
				return
			}
			if r.serveSpill(w, req, modelID) {
				return
			}
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	if !incomingPeer && r.serveSlotSpill(w, req, modelID) {
		return
	}
	if err := r.proxyLocal(w, req, modelID); err != nil {
		if !incomingPeer && r.serveSpill(w, req, modelID) {
			return
		}
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
}

func (r *Router) servePool(w http.ResponseWriter, req *http.Request, poolName string) {
	pool := r.cfg.Pool(poolName)
	if pool == nil {
		http.Error(w, fmt.Sprintf("unknown pool %q", poolName), http.StatusServiceUnavailable)
		return
	}
	skip := map[string]bool{}
	var last error
	for {
		tgt, err := r.pickPoolTarget(pool, skip)
		if err != nil {
			if last != nil {
				http.Error(w, last.Error(), http.StatusServiceUnavailable)
				return
			}
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		skip[occupancyKey(tgt)] = true
		var serveErr error
		switch {
		case tgt.Local != "":
			serveErr = r.proxyLocalHeld(w, req, tgt.Local)
		case tgt.Peer != "":
			serveErr = r.proxyPeer(w, req, tgt.Peer, tgt.PeerID)
		default:
			serveErr = fmt.Errorf("no router for requested model")
		}
		r.releaseOccupancy(tgt)
		if serveErr == nil {
			return
		}
		last = serveErr
	}
}

// serveSpill tries later pool targets after a direct local id could not start,
// then any peer that advertises the same model_id. Incoming peer-proxied
// requests do not bounce (X-Model-Router-Peer).
func (r *Router) serveSpill(w http.ResponseWriter, req *http.Request, failedLocal string) bool {
	for _, raw := range r.spillTargets(failedLocal) {
		ref := resolveRef(raw, r.cfg)
		switch {
		case ref.Local != "" && ref.Local != failedLocal:
			if !r.localCanServe(r.cfg.Stanza(ref.Local)) {
				continue
			}
			if err := r.proxyLocal(w, req, ref.Local); err == nil {
				return true
			}
		case ref.Peer != "":
			if !r.peerEligible(ref.Peer, ref.PeerID) {
				continue
			}
			if err := r.proxyPeer(w, req, ref.Peer, ref.PeerID); err == nil {
				return true
			}
		}
	}
	tgt, err := r.resolveMesh(failedLocal)
	if err != nil || tgt.Peer == "" {
		return false
	}
	r.logger.Meshf("spill %s -> %s", failedLocal, tgt.Peer)
	return r.proxyPeer(w, req, tgt.Peer, tgt.PeerID) == nil
}

// serveSlotSpill sends a request to a peer that already has a free slot (or
// can admit a fresh load) when local occupancy is at --parallel. Incoming
// peer-proxied requests do not bounce. If nothing can take it, the caller
// queues on the local backend.
func (r *Router) serveSlotSpill(w http.ResponseWriter, req *http.Request, localID string) bool {
	if req.Header.Get("X-Model-Router-Peer") != "" {
		return false
	}
	tgt, err := r.pickPeerForModel(localID, true)
	if err != nil || tgt.Peer == "" {
		return false
	}
	r.logger.Meshf("slot spill %s -> %s", localID, tgt.Peer)
	return r.proxyPeer(w, req, tgt.Peer, tgt.PeerID) == nil
}

func (r *Router) spillTargets(failedLocal string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range r.cfg.Pools {
		found := false
		for _, t := range p.Targets {
			ref := resolveRef(t, r.cfg)
			if !found {
				if ref.Local == failedLocal {
					found = true
				}
				continue
			}
			if seen[t] {
				continue
			}
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// serveLocal proxies to a local backend, spawning it if needed.
func (r *Router) serveLocal(w http.ResponseWriter, req *http.Request, modelID string) {
	if err := r.proxyLocal(w, req, modelID); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
}

// proxyLocal serves req on the best instance of modelID, queueing on the
// least-loaded one when every instance is busy.
func (r *Router) proxyLocal(w http.ResponseWriter, req *http.Request, modelID string) error {
	s := r.cfg.Stanza(modelID)
	if s == nil {
		return fmt.Errorf("unknown local model %q", modelID)
	}
	key, ok := r.acquireInstance(s, true)
	if !ok {
		return fmt.Errorf("router shutting down")
	}
	defer r.releaseOccupancy(Target{Local: key})
	return r.proxyLocalHeld(w, req, key)
}

// proxyLocalHeld loads instance key if needed and proxies to it. The caller
// already holds an occupancy slot on key.
func (r *Router) proxyLocalHeld(w http.ResponseWriter, req *http.Request, key string) error {
	if _, err := r.load(req.Context(), key, true); err != nil {
		return err
	}
	r.mu.Lock()
	m := r.managed[key]
	r.mu.Unlock()
	if m == nil || m.State() != StateRunning {
		return fmt.Errorf("model failed to start")
	}
	m.markUsed()
	defer m.markUsed()
	r.proxyTo(w, req, m.proxyURL, key, r.node)
	return nil
}

// servePeer proxies to a peer, first asking it to load the model (the peer runs
// its own admission control — no split-brain).
func (r *Router) servePeer(w http.ResponseWriter, req *http.Request, peerName, peerModel string) {
	if err := r.proxyPeer(w, req, peerName, peerModel); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
}

func (r *Router) proxyPeer(w http.ResponseWriter, req *http.Request, peerName, peerModel string) error {
	p := r.cfg.Peer(peerName)
	if p == nil {
		return fmt.Errorf("unknown peer %q", peerName)
	}
	// OpenAI-kind peers (openrouter etc.) have no /_router control API and no
	// load step — the upstream serves the model id directly.
	if p.Kind != "openai" {
		if err := r.peerLoad(req.Context(), p, peerModel); err != nil {
			return err
		}
	}
	// The request may still name the model as "peerName/peerModel"; the peer
	// only knows its local id, so rewrite the model field before proxying
	// (llama-swap's ReplaceRequestModel does the same). The field rewritten is
	// the peer's configured body_field, not a hardcoded "model".
	if err := rewriteBodyModel(req, peerModel, p.BodyField); err != nil {
		return err
	}
	req.Header.Set("X-Model-Router-Peer", r.node)
	r.logger.Meshf("%s %s -> %s (%s)", req.Method, req.URL.Path, peerName, peerModel)
	r.proxyTo(w, req, p.BaseURL, peerModel, peerName)
	return nil
}

// rewriteBodyModel replaces the model field in a request body (or query) with
// the given id, restoring the body for downstream use. JSON bodies are
// rewritten to field (the peer's configured body_field), preserving all other
// fields; GETs rewrite the "model" query param, which body_field does not
// cover. The matcher already buffered and restored r.Body; this only sets
// the field on those bytes.
func rewriteBodyModel(req *http.Request, newModel, field string) error {
	if req.Method == http.MethodGet {
		q := req.URL.Query()
		if q.Get("model") != "" {
			q.Set("model", newModel)
			req.URL.RawQuery = q.Encode()
		}
		return nil
	}
	ct := req.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		return nil
	}
	if req.Body == nil {
		return nil
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(req.Body); err != nil {
		return err
	}
	raw := buf.Bytes()
	if len(raw) == 0 {
		return nil
	}
	rewritten, ok := jsonSetField(raw, field, newModel)
	if !ok {
		req.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	req.Body = io.NopCloser(bytes.NewReader(rewritten))
	req.ContentLength = int64(len(rewritten))
	return nil
}

// jsonSetField sets field to value on a JSON object. false means the bytes
// are not an object we can rewrite; callers forward them unchanged.
func jsonSetField(raw []byte, field, value string) ([]byte, bool) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	if obj == nil {
		obj = map[string]any{}
	}
	obj[field] = value
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// peerLoad asks a peer to load a model. The peer's own admission control
// decides; we only use cached telemetry for *candidate* selection.
func (r *Router) peerLoad(ctx context.Context, p *config.Peer, modelID string) error {
	body, _ := json.Marshal(map[string]string{"model_id": modelID})
	url := p.BaseURL + "/_router/load"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("peer %s load: %w", p.Name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("peer %s load: %w", p.Name, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer %s rejected load of %s (%d)", p.Name, modelID, resp.StatusCode)
	}
	return nil
}

// proxyTo reverse-proxies the request to baseURL, preserving path and body and
// streaming SSE responses. Buffering is disabled for streaming.
func (r *Router) proxyTo(w http.ResponseWriter, req *http.Request, baseURL, model, target string) {
	u, err := url.Parse(baseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	wrapped, done := r.wrapProxy(w, req, model, target)
	defer done()
	// The matcher already read the body and restored r.Body with a fresh
	// reader, so the ReverseProxy can stream it downstream.
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			r.Host = u.Host
			// Keep the original path (peer router / backend routes on it).
			r.URL.Path = req.URL.Path
			r.URL.RawQuery = req.URL.RawQuery
		},
		FlushInterval: -1, // flush immediately for SSE
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
	// Tell any fronting proxy (nginx on gareth) not to buffer SSE.
	wrapped.Header().Set("X-Accel-Buffering", "no")
	proxy.ServeHTTP(wrapped, req)
}

// Preload loads the configured startup models in order.
func (r *Router) Preload() {
	for _, id := range r.cfg.Preload {
		if r.cfg.Stanza(id) == nil {
			r.logger.Errorf("preload: unknown model %q", id)
			continue
		}
		r.logger.Infof("preloading %s", id)
		if _, err := r.Load(id); err != nil {
			r.logger.Errorf("preload %s failed: %v", id, err)
		}
	}
}

// ModelStatus is one unique model id for /v1/models and /_router/status.
type ModelStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Type      string `json:"type,omitempty"`   // always "model"
	Origin    string `json:"origin,omitempty"` // local | remote
	State     string `json:"state,omitempty"`
	Freshness string `json:"freshness,omitempty"` // fresh | stale when running locally
	VramMB    int64  `json:"vram_mb,omitempty"`
	APIType   string `json:"api_type,omitempty"`
	// GPUs lists where running local instances sit, one entry per instance.
	GPUs []int `json:"gpus,omitempty"`
}

// LocalModelStatuses lists unique mesh models (local + reachable remotes).
func (r *Router) LocalModelStatuses() []ModelStatus {
	return r.catalogStatuses()
}

// AllModelStatuses is the operator catalog; same unique mesh list as /v1/models.
func (r *Router) AllModelStatuses() []ModelStatus {
	return r.catalogStatuses()
}

func (r *Router) catalogStatuses() []ModelStatus {
	var out []ModelStatus
	seen := map[string]bool{}
	now := time.Now()
	for _, id := range r.cfg.StanzaIDs() {
		s := r.cfg.Stanza(id)
		var insts []*Managed
		r.mu.Lock()
		for n := 0; n < s.InstanceCount(); n++ {
			if m := r.managed[instanceKey(id, n)]; m != nil {
				insts = append(insts, m)
			}
		}
		r.mu.Unlock()
		// The model reads as running if any instance is, and fresh if any
		// running instance is.
		st := ProcessState(StateStopped)
		freshness := ""
		var gpus []int
		for _, m := range insts {
			ms := m.State()
			if ms != StateRunning {
				if st != StateRunning {
					st = ms
				}
				continue
			}
			st = StateRunning
			gpus = append(gpus, m.gpuIndex())
			if !m.IsStale(now) {
				freshness = "fresh"
			} else if freshness == "" {
				freshness = "stale"
			}
		}
		out = append(out, ModelStatus{
			ID: id, Name: s.Name, Type: "model", Origin: "local",
			State: string(st), Freshness: freshness, VramMB: s.VramMB, APIType: s.APIType,
			GPUs: gpus,
		})
		seen[id] = true
		for _, a := range s.Aliases {
			seen[a] = true
		}
	}
	for i := range r.cfg.Peers {
		p := &r.cfg.Peers[i]
		if p.Kind != "openai" && !r.peers.Fresh(p.Name) {
			continue
		}
		for _, m := range p.Models {
			if seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, ModelStatus{ID: m, Type: "model", Origin: "remote", State: "available"})
		}
	}
	return out
}

// TelemetrySnapshot assembles this node's telemetry for peers: live nvidia-smi
// free VRAM, projected free if stale models were evicted, and each loaded
// model marked fresh or stale.
func (r *Router) TelemetrySnapshot() *Telemetry {
	gpus := r.ledger.GPUs()
	now := time.Now()
	staleByGPU := map[int]int64{}
	idleByGPU := map[int]int64{}
	r.mu.Lock()
	type running struct {
		id       string
		m        *Managed
		inFlight int
	}
	var live []running
	for id, m := range r.managed {
		live = append(live, running{id: id, m: m, inFlight: r.occupancy[id]})
	}
	r.mu.Unlock()
	var loaded []LoadedModel
	for _, item := range live {
		if item.m.State() != StateRunning {
			continue
		}
		gpu := item.m.gpuIndex()
		vram := r.modelVram(item.id)
		freshness := "fresh"
		if item.m.IsStale(now) {
			freshness = "stale"
			if gpu >= 0 {
				staleByGPU[gpu] += vram
			}
		}
		slots := 1
		if s := r.stanzaOf(item.id); s != nil {
			slots = s.SlotCount()
		}
		lm := LoadedModel{
			ID: instanceModel(item.id), Freshness: freshness, VramMB: vram, GPU: gpu,
			Slots: slots, InFlight: item.inFlight,
		}
		if lm.ID != item.id {
			lm.Instance = item.id
		}
		loaded = append(loaded, lm)
		if gpu >= 0 && item.inFlight == 0 {
			idleByGPU[gpu] += vram
		}
	}
	for _, g := range gpus {
		reclaim := staleByGPU[g.Index]
		g.FreeIfStaleEvictedMB = g.FreeMB + reclaim
		if g.TotalMB > 0 && g.FreeIfStaleEvictedMB > g.TotalMB {
			g.FreeIfStaleEvictedMB = g.TotalMB
		}
		g.FreeIfIdleEvictedMB = g.FreeMB + idleByGPU[g.Index]
		if g.TotalMB > 0 && g.FreeIfIdleEvictedMB > g.TotalMB {
			g.FreeIfIdleEvictedMB = g.TotalMB
		}
	}
	return &Telemetry{
		Node:         r.node,
		GPUs:         gpus,
		LoadedModels: loaded,
		Timestamp:    now.Unix(),
	}
}

// BackendMetric is one running backend's /metrics scrape for the operator UI.
type BackendMetric struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// BackendMetrics scrapes each running local backend's /metrics. llama.cpp
// stanzas that pass --metrics return Prometheus text; sd.cpp usually does not.
func (r *Router) BackendMetrics() []BackendMetric {
	r.mu.Lock()
	type run struct {
		id   string
		port int
	}
	var live []run
	for id, m := range r.managed {
		if m.State() == StateRunning && m.port > 0 {
			live = append(live, run{id: id, port: m.port})
		}
	}
	r.mu.Unlock()
	sort.Slice(live, func(i, j int) bool { return live[i].id < live[j].id })
	client := &http.Client{Timeout: 800 * time.Millisecond}
	out := make([]BackendMetric, 0, len(live))
	for _, item := range live {
		url := fmt.Sprintf("http://127.0.0.1:%d/metrics", item.port)
		resp, err := client.Get(url)
		if err != nil {
			out = append(out, BackendMetric{ID: item.id, Body: err.Error()})
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		text := strings.TrimSpace(string(body))
		if text == "" {
			text = fmt.Sprintf("HTTP %d (empty)", resp.StatusCode)
		}
		out = append(out, BackendMetric{ID: item.id, Body: text})
	}
	return out
}

// HandleLoad is the control endpoint handler for peer load() calls. Accepts
// stanza ids and aliases (so a peer can load "coding-model" on a node whose
// stanza is "qwopus-27b-coder").
func loadWaitTimeout(s *config.Stanza) time.Duration {
	spin := time.Duration(s.SpinUpSeconds) * time.Second
	if spin < 90*time.Second {
		spin = 90 * time.Second
	}
	return spin + 8*time.Minute
}

func (r *Router) HandleLoad(_ context.Context, modelID string) error {
	s := r.cfg.Stanza(modelID)
	if s == nil {
		s = r.cfg.Alias(modelID)
	}
	if s == nil {
		return fmt.Errorf("unknown local model %q", modelID)
	}
	// Detach from the inbound request so a client abort does not cancel a
	// useful wait/evict/spin. Process shutdown still cancels the wait.
	loadCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		timer := time.NewTimer(loadWaitTimeout(s))
		defer timer.Stop()
		select {
		case <-r.shutdown:
			cancel()
		case <-timer.C:
			cancel()
		case <-loadCtx.Done():
		}
	}()
	// Any live instance satisfies a peer; join it rather than start a copy.
	key := instanceKey(s.ModelID, 0)
	r.mu.Lock()
	if live := r.liveInstancesLocked(s); len(live) > 0 {
		key = live[0]
	}
	r.mu.Unlock()
	_, err := r.load(loadCtx, key, true)
	return err
}
