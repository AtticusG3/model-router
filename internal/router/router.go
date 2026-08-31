package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
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
	spawn     spawner             // tests inject a fake; nil uses the GOOS default
	admitWait chan struct{}       // 1-buffer poke when occupancy/unload may free VRAM

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
		admitWait: make(chan struct{}, 1),
		httpc:     &http.Client{Timeout: 0}, // no overall timeout; streaming is long
		activity:  NewActivityLog(),
	}
}

// Activity returns the in-memory generation request log for the operator UI.
func (r *Router) Activity() *ActivityLog { return r.activity }

// Ledger returns the admission ledger (used by the telemetry poller).
func (r *Router) Ledger() *Ledger { return r.ledger }

// Peers returns the peer cache (used by the peer syncer and status).
func (r *Router) Peers() *PeerCache { return r.peers }

// Load ensures the given local stanza is running. Fail-fast: preload and
// tests use this. Request paths use load(..., true) so a busy GPU queues
// instead of 503.
func (r *Router) Load(modelID string) (int, error) {
	return r.load(context.Background(), modelID, false)
}

func (r *Router) load(ctx context.Context, modelID string, wait bool) (int, error) {
	s := r.cfg.Stanza(modelID)
	if s == nil {
		return -1, fmt.Errorf("unknown local model %q", modelID)
	}

	loggedWait := false
	for {
		r.mu.Lock()
		m, ok := r.managed[modelID]
		if !ok {
			m = newManaged(s, s.Port, r.logger)
			if r.spawn != nil {
				m.spawn = r.spawn
			}
			r.managed[modelID] = m
		}
		r.mu.Unlock()

		if m.State() == StateRunning {
			return m.gpuIndex(), nil
		}

		// Admission control: reserve VRAM before spawning. Keep it until Unload
		// or failed Start so crash-restart does not need to re-reserve.
		gpu, ok := r.ledger.Reserve(s)
		if !ok {
			r.evictToFit(s)
			gpu, ok = r.ledger.Reserve(s)
		}
		if ok {
			if _, err := m.Start(); err != nil {
				r.ledger.Release(modelID)
				r.forgetManaged(modelID, m)
				if !wait || !r.canEventuallyAdmit(s) || !startRetryable(err) {
					return -1, err
				}
				r.logger.Infof("retrying load of %s after %v", modelID, err)
				if !loggedWait {
					r.logger.Infof("waiting for %d MB for %s (GPU busy)", s.VramMB, modelID)
					loggedWait = true
				}
				if err := r.waitAdmit(ctx); err != nil {
					return -1, err
				}
				continue
			}
			m.SetGPU(gpu)
			return gpu, nil
		}
		if !wait || !r.canEventuallyAdmit(s) {
			return -1, fmt.Errorf("no GPU has %d MB free for %s", s.VramMB, modelID)
		}
		if !loggedWait {
			r.logger.Infof("waiting for %d MB for %s (GPU busy)", s.VramMB, modelID)
			loggedWait = true
		}
		if err := r.waitAdmit(ctx); err != nil {
			return -1, err
		}
	}
}

// canEventuallyAdmit is true unless we know the pinned (or every) card is
// smaller than s. Missing telemetry and a missing pinned GPU wait rather
// than 503: occupied VRAM can free after idle eviction.
func (r *Router) canEventuallyAdmit(s *config.Stanza) bool {
	want := deviceIndex(s.Device)
	gpus := r.ledger.GPUs()
	if len(gpus) == 0 {
		return true
	}
	saw := false
	for _, g := range gpus {
		if want >= 0 && g.Index != want {
			continue
		}
		saw = true
		if g.TotalMB <= 0 || g.TotalMB >= s.VramMB {
			return true
		}
	}
	return want >= 0 && !saw
}

func startRetryable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "stopped during start") ||
		strings.Contains(s, "exited during start") ||
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

// Unload stops a local model and releases its VRAM reservation. A ledger
// reservation without a process still gets released so eviction cannot 503
// on a ghost occupant.
func (r *Router) Unload(modelID string) error {
	r.mu.Lock()
	m, ok := r.managed[modelID]
	delete(r.managed, modelID)
	r.mu.Unlock()
	gpu := r.ledger.reservedGPU(modelID)
	if m != nil {
		if g := m.gpuIndex(); g >= 0 {
			gpu = g
		}
		m.Stop()
	}
	vram := r.modelVram(modelID)
	had := gpu >= 0 || ok
	r.ledger.Release(modelID)
	r.ledger.Credit(gpu, vram)
	r.pokeAdmit()
	if !had {
		return fmt.Errorf("not loaded: %s", modelID)
	}
	return nil
}

// SweepStale actively evicts running models whose idle TTL has expired.
// Lazy eviction (evictToFit) only reclaims VRAM when a new load needs it;
// this sweep makes ttl a real eviction deadline so VRAM returns to the
// system (e.g. for hashcat's governor to see) even with no incoming load.
// Models with ttl <= 0 are resident and never swept. In-flight requests,
// starting/stopping processes are protected.
func (r *Router) SweepStale() {
	for _, id := range r.cfg.StanzaIDs() {
		if !r.modelStale(id) {
			continue
		}
		if r.protected(id) {
			continue
		}
		r.logger.Infof("[router] idle TTL expired: evicting %s", id)
		if err := r.Unload(id); err != nil {
			r.logger.Errorf("[router] ttl evict %s: %v", id, err)
		}
	}
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

// evictToFit unloads occupants until the ledger can admit s.
// Pass 1: stale models. Pass 2 (last resort): any idle model that is not
// mid-generation or starting, including ttl=0 residents.
func (r *Router) evictToFit(s *config.Stanza) {
	want := deviceIndex(s.Device)
	r.evictPass(s, want, true)
	if r.ledger.CanAdmit(s) {
		return
	}
	r.evictPass(s, want, false)
}

func (r *Router) evictPass(s *config.Stanza, gpu int, staleOnly bool) {
	for _, id := range r.ledger.occupantsOnGPU(gpu, s.ModelID) {
		if r.protected(id) {
			continue
		}
		if staleOnly && !r.modelStale(id) {
			continue
		}
		why := "idle"
		if staleOnly {
			why = "stale"
		}
		r.logger.Infof("evicting %s %s to admit %s", why, id, s.ModelID)
		if err := r.Unload(id); err != nil {
			r.logger.Errorf("evict %s: %v", id, err)
		}
		if r.ledger.CanAdmit(s) {
			return
		}
	}
}

func (r *Router) modelStale(id string) bool {
	r.mu.Lock()
	m := r.managed[id]
	r.mu.Unlock()
	if m == nil {
		return false
	}
	return m.IsStale(time.Now())
}

// protected is true while a request is mid-generation, the process is still
// spinning up, or Unload is already stopping it. Evicting a start races two
// loads into "stopped during start" 503s; wait instead.
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
	r.mu.Lock()
	r.occupancy[id]++
	r.mu.Unlock()
}

func (r *Router) occupancyOf(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.occupancy[id]
}

// tryHoldLocalSlot increments occupancy when it is below the stanza's slot
// cap. False means the local backend is full; the caller should spill or
// queue.
func (r *Router) tryHoldLocalSlot(id string) bool {
	slots := 1
	if s := r.cfg.Stanza(id); s != nil {
		slots = s.SlotCount()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.occupancy[id] >= slots {
		return false
	}
	r.occupancy[id]++
	return true
}

func (r *Router) localCanServe(s *config.Stanza) bool {
	if s == nil {
		return false
	}
	r.mu.Lock()
	m := r.managed[s.ModelID]
	running := m != nil && m.State() == StateRunning
	r.mu.Unlock()
	if running {
		return true
	}
	var evict []string
	blocked := false
	for _, id := range r.ledger.occupantsOnGPU(deviceIndex(s.Device), s.ModelID) {
		if r.protected(id) {
			blocked = true
			continue
		}
		evict = append(evict, id)
	}
	if r.ledger.CanAdmitEvicting(s, evict) {
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
	for _, m := range t.LoadedModels {
		if m.ID != modelID {
			continue
		}
		slots := m.Slots
		if slots <= 0 {
			slots = 1
		}
		return true, m.InFlight < slots, true
	}
	return false, false, true
}

func (r *Router) modelVram(modelID string) int64 {
	if s := r.cfg.Stanza(modelID); s != nil {
		return s.VramMB
	}
	if s := r.cfg.Alias(modelID); s != nil {
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
	if !incomingPeer && r.tryHoldLocalSlot(modelID) {
		err := r.proxyLocalHeld(w, req, modelID)
		r.releaseOccupancy(Target{Local: modelID})
		if err == nil {
			return
		}
		if r.serveSpill(w, req, modelID) {
			return
		}
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
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

func (r *Router) proxyLocal(w http.ResponseWriter, req *http.Request, modelID string) error {
	r.holdOccupancy(modelID)
	defer r.releaseOccupancy(Target{Local: modelID})
	return r.proxyLocalHeld(w, req, modelID)
}

func (r *Router) proxyLocalHeld(w http.ResponseWriter, req *http.Request, modelID string) error {
	if _, err := r.load(req.Context(), modelID, true); err != nil {
		return err
	}
	r.mu.Lock()
	m := r.managed[modelID]
	r.mu.Unlock()
	if m == nil || m.State() != StateRunning {
		return fmt.Errorf("model failed to start")
	}
	m.markUsed()
	defer m.markUsed()
	r.proxyTo(w, req, m.stanza.Proxy, modelID, r.node)
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
	for _, id := range r.cfg.StanzaIDs() {
		s := r.cfg.Stanza(id)
		r.mu.Lock()
		m, ok := r.managed[id]
		st := ProcessState(StateStopped)
		if ok {
			st = m.State()
		}
		r.mu.Unlock()
		freshness := ""
		if ok && st == StateRunning {
			if m.IsStale(time.Now()) {
				freshness = "stale"
			} else {
				freshness = "fresh"
			}
		}
		out = append(out, ModelStatus{
			ID: id, Name: s.Name, Type: "model", Origin: "local",
			State: string(st), Freshness: freshness, VramMB: s.VramMB, APIType: s.APIType,
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
		if s := r.cfg.Stanza(item.id); s != nil {
			slots = s.SlotCount()
		}
		loaded = append(loaded, LoadedModel{
			ID: item.id, Freshness: freshness, VramMB: vram, GPU: gpu,
			Slots: slots, InFlight: item.inFlight,
		})
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
		if m.State() == StateRunning && m.stanza != nil && m.stanza.Port > 0 {
			live = append(live, run{id: id, port: m.stanza.Port})
		}
	}
	r.mu.Unlock()
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
	// useful wait/evict/spin. The model stays up for the next request.
	loadCtx, cancel := context.WithTimeout(context.Background(), loadWaitTimeout(s))
	defer cancel()
	_, err := r.load(loadCtx, s.ModelID, true)
	return err
}
