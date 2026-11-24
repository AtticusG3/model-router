package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"model-router/internal/config"
)

// Router is one node's full instance of the model router. It owns the local
// backends, the admission ledger, and the peer cache.
type Router struct {
	cfgp   atomic.Pointer[config.Config] // swapped whole by Apply; read via cfg()
	logger *Logger
	ledger *Ledger
	peers  *PeerCache
	node   string

	// disc is the discovered peer set, merged beside the immutable config
	// peers. discMiss counts consecutive scans since a peer last answered.
	discMu   sync.Mutex
	disc     map[string]*config.Peer
	discMiss map[string]int

	applyMu   sync.Mutex // one Apply at a time
	mu        sync.Mutex
	managed   map[string]*Managed  // local stanza id -> supervisor
	occupancy map[string]int       // target key -> in-flight selected requests
	draining  map[string]bool      // claimed for unload; new requests wait
	speaking  map[string]int       // instance key -> speech requests in its backend (slot cap)
	peerUntil map[string]time.Time // "peer/model" -> end of the Retry-After that peer sent
	jobs      map[string]*job      // async job id -> owner; see jobs.go
	jobsDone  []*job               // finished jobs in the order they ended; may hold stale entries
	spawn     spawner              // tests inject a fake; nil uses the GOOS default
	admitWait chan struct{}        // 1-buffer poke when occupancy/unload may free VRAM
	shutdown  chan struct{}
	shutOnce  sync.Once
	reloader  *config.Reloader // set once at startup, before the server takes requests; nil in most tests

	coolMu    sync.Mutex
	cooldowns map[string]*startCooldown // stanza model id -> failed-start state

	// evictedPreloads maps a preload model that lost its VRAM to an eviction
	// to the instance key that evicted it, so restorePreloads can bring it
	// back once the room returns. Guarded by mu.
	evictedPreloads map[string]string
	// restoreMu keeps one restore pass in flight; a slow preload start must
	// not be restarted by the next sweep or unload.
	restoreMu sync.Mutex

	httpc    *http.Client
	activity *ActivityLog
	metrics  *Metrics
	// imageSteps builds the steps a stanza's image_pipeline runs; tests swap it.
	imageSteps func(*config.Stanza) []imageStep

	// voiceMu serialises voice library writes so two uploads of one id cannot
	// interleave their clip and sidecar.
	voiceMu sync.Mutex

	// meta is the last good catalog.metadata_url file; nil when off or not yet fetched.
	meta atomic.Pointer[metadataFile]
	// hub is the last good catalog.hub data by "<hub>:<repo>"; nil when off.
	hub atomic.Pointer[map[string]hubEntry]
}

// New builds a Router from parsed config.
func New(cfg *config.Config, logger *Logger, node string) *Router {
	stale := time.Duration(cfg.Telemetry.PeerStaleSeconds) * time.Second
	ledger := NewLedger()
	ledger.headroomMB = cfg.HeadroomMB
	r := &Router{
		logger:          logger,
		ledger:          ledger,
		peers:           NewPeerCache(stale),
		disc:            map[string]*config.Peer{},
		discMiss:        map[string]int{},
		node:            node,
		managed:         map[string]*Managed{},
		occupancy:       map[string]int{},
		draining:        map[string]bool{},
		speaking:        map[string]int{},
		peerUntil:       map[string]time.Time{},
		jobs:            map[string]*job{},
		cooldowns:       map[string]*startCooldown{},
		evictedPreloads: map[string]string{},
		admitWait:       make(chan struct{}, 1),
		shutdown:        make(chan struct{}),
		httpc:           &http.Client{Timeout: 0}, // no overall timeout; streaming is long
		activity:        NewActivityLog(),
		metrics:         NewMetrics(),
	}
	r.imageSteps = r.defaultImageSteps
	r.cfgp.Store(cfg)
	return r
}

// cfg is the running config. Apply replaces it whole, so a request reads one
// consistent catalog; take it once per decision rather than re-reading.
func (r *Router) cfg() *config.Config { return r.cfgp.Load() }

// Metrics returns the request counters and latency histograms behind /metrics.
func (r *Router) Metrics() *Metrics { return r.metrics }

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

// PeerList is every peer in play: configured peers first, in config order,
// then discovered peers sorted by node name. A configured peer shadows a
// discovered peer with the same name.
func (r *Router) PeerList() []*config.Peer {
	cfg := r.cfg()
	out := make([]*config.Peer, 0, len(cfg.Peers))
	configured := make(map[string]bool, len(cfg.Peers))
	for i := range cfg.Peers {
		out = append(out, &cfg.Peers[i])
		configured[cfg.Peers[i].Name] = true
	}
	r.discMu.Lock()
	names := make([]string, 0, len(r.disc))
	for name := range r.disc {
		if !configured[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, r.disc[name])
	}
	r.discMu.Unlock()
	return out
}

// peerByName returns a configured or discovered peer, or nil.
func (r *Router) peerByName(name string) *config.Peer {
	if p := r.cfg().Peer(name); p != nil {
		return p
	}
	r.discMu.Lock()
	defer r.discMu.Unlock()
	return r.disc[name]
}

// DiscoveredPeers returns the discovered peer names, sorted.
func (r *Router) DiscoveredPeers() []string {
	r.discMu.Lock()
	defer r.discMu.Unlock()
	out := make([]string, 0, len(r.disc))
	for name := range r.disc {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// discoveredResult is one scanned node: the peer to advertise and the
// telemetry the probe returned.
type discoveredResult struct {
	peer      *config.Peer
	telemetry *Telemetry
}

// applyDiscovery merges one scan's findings into the runtime set. Peers found
// before are refreshed; a discovered peer absent for discoveryDropAfter scans
// is dropped. configured lists nodes a configured peer already owns; those are
// never discovered and shadow any earlier detected copy.
func (r *Router) applyDiscovery(found map[string]discoveredResult, configured map[string]bool) (added, dropped int) {
	r.discMu.Lock()
	defer r.discMu.Unlock()
	for name := range configured {
		if _, ok := r.disc[name]; ok {
			delete(r.disc, name)
			delete(r.discMiss, name)
			r.peers.Delete(name)
		}
	}
	for name, res := range found {
		if _, ok := r.disc[name]; !ok {
			added++
			r.logger.Meshf("discovered peer %s at %s", name, res.peer.BaseURL)
		}
		r.disc[name] = res.peer
		r.discMiss[name] = 0
		if res.telemetry != nil {
			r.peers.Set(name, res.telemetry)
		}
	}
	for name := range r.disc {
		if _, ok := found[name]; ok {
			continue
		}
		r.discMiss[name]++
		if r.discMiss[name] < discoveryDropAfter {
			continue
		}
		delete(r.disc, name)
		delete(r.discMiss, name)
		r.peers.Delete(name)
		dropped++
		r.logger.Meshf("dropped discovered peer %s (stale)", name)
	}
	return added, dropped
}

// clearDiscovery drops every discovered peer, e.g. when a reload turns
// discovery off. Configured peers are untouched.
func (r *Router) clearDiscovery() (dropped int) {
	r.discMu.Lock()
	defer r.discMu.Unlock()
	for name := range r.disc {
		delete(r.disc, name)
		delete(r.discMiss, name)
		r.peers.Delete(name)
		dropped++
		r.logger.Meshf("dropped discovered peer %s (discovery off)", name)
	}
	return dropped
}

// Load ensures the given local stanza (or "model#n" instance) is running.
// Fail-fast: preload and tests use this. Request paths use load(..., true)
// so a busy GPU queues instead of 503.
func (r *Router) Load(modelID string) (int, error) {
	return r.load(context.Background(), modelID, false, true)
}

// load ensures instance key is running. evict false keeps a caller that must
// not disturb other models (restoring an evicted preload) from unloading one.
func (r *Router) load(ctx context.Context, key string, wait, evict bool) (int, error) {
	s := r.cfg().Stanza(instanceModel(key))
	if n := instanceIndex(key); s == nil || n < 0 || n >= s.InstanceCount() {
		return -1, fmt.Errorf("unknown local model %q", key)
	}
	// An extra copy is opportunistic: if its GPU is gone, the caller queues
	// on a sibling instead of evicting or waiting here.
	evictOK := evict && !r.hasLiveSibling(key)
	if !evictOK {
		wait = false
	}

	loggedWait := false
	var deadline time.Time
	hardRetries := 0
	var lastErr error
	// waitCtx carries the request's admit deadline so waitAdmit unblocks when
	// the budget ends, not just when its 200 ms tick fires.
	waitCtx := ctx
	if d, ok := ctx.Value(admitDeadlineKey{}).(time.Time); ok {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithDeadline(ctx, d)
		defer cancel()
	}
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
		// A caller that must not cold-load its model (the caption step) gets an
		// error here rather than a start that could evict a neighbour.
		if _, noLoad := ctx.Value(noLoadKey{}).(bool); noLoad {
			return -1, fmt.Errorf("%s is not running", key)
		}
		// Inside a start cooldown nothing is spawned and nothing is evicted.
		if err := r.startRefused(s.ModelID); err != nil {
			return -1, err
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
				// A stop (unload, eviction) is not this model's failure. Any other
				// failure that will not be retried opens the start cooldown.
				willRetry := wait && hardStartFailure(err) && hardRetries == 0 && r.canEventuallyAdmit(s)
				if !willRetry && !stoppedDuringStart(err) {
					return -1, r.startFailed(s.ModelID, err)
				}
				if !wait || !r.canEventuallyAdmit(s) {
					return -1, err
				}
				if hardStartFailure(err) {
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
			r.startHealthy(s.ModelID)
			return gpu, nil
		}
		if hardRetries > 0 || !wait || !r.canEventuallyAdmit(s) {
			if !evictOK {
				r.forgetManaged(key, m)
			}
			if hardRetries > 0 {
				// The retry could not reserve: the first failure stands.
				return -1, r.startFailed(s.ModelID, lastErr)
			}
			if lastErr != nil {
				return -1, lastErr
			}
			return -1, &admitError{fmt.Sprintf("no GPU has %d MB free for %s", s.VramMB, key)}
		}
		if !loggedWait {
			r.logger.Infof("waiting for %d MB for %s (GPU busy)", s.VramMB, key)
			loggedWait = true
			deadline = time.Now().Add(r.cfg().AdmitTimeout())
			// A request that already waited on peers keeps one admit budget.
			if d, ok := ctx.Value(admitDeadlineKey{}).(time.Time); ok && d.Before(deadline) {
				deadline = d
			}
		}
		if time.Now().After(deadline) {
			r.logger.Warnf("gave up waiting for %d MB for %s (GPU busy)", s.VramMB, key)
			return -1, &admitError{fmt.Sprintf("timed out waiting for %d MB for %s (GPU busy)", s.VramMB, key)}
		}
		if err := r.waitAdmit(waitCtx); err != nil {
			if time.Now().After(deadline) {
				r.logger.Warnf("gave up waiting for %d MB for %s (GPU busy)", s.VramMB, key)
				return -1, &admitError{fmt.Sprintf("timed out waiting for %d MB for %s (GPU busy)", s.VramMB, key)}
			}
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

// pinnedMB is the VRAM pinned models other than s hold on gpu. A pinned
// stanza is never evicted, so that room never comes back.
func (r *Router) pinnedMB(gpu int, s *config.Stanza) int64 {
	var mb int64
	for _, id := range r.ledger.occupantsOnGPU(gpu, "") {
		if o := r.stanzaOf(id); o != nil && o.Pin && o.ModelID != s.ModelID {
			mb += r.ledger.reservationUsed(id)[gpu]
		}
	}
	return mb
}

// canEventuallyAdmit is true unless we know the pinned (or every) card is
// smaller than s after headroom and the VRAM pinned models hold. Missing
// telemetry and a missing pinned GPU wait rather than 503: other occupied VRAM
// can free after idle eviction.
func (r *Router) canEventuallyAdmit(s *config.Stanza) bool {
	idxs := deviceIndexes(s.Device)
	gpus := r.ledger.GPUs()
	if len(gpus) == 0 {
		return true
	}
	headroom := r.ledger.headroomMB
	usable := func(total int64) int64 {
		return max(total-headroom, 0)
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
			if usable(totals[idx])-r.pinnedMB(idx, s) < need {
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
		if total <= 0 || usable(total)-r.pinnedMB(idx, s) >= s.VramMB {
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
	s := r.cfg().Stanza(id)
	if s == nil || s.InstanceCount() == 1 {
		if err := r.unloadInstance(id); err != nil {
			return err
		}
		// A model the operator stopped frees its card for a pending preload.
		// Eviction unloads do not come through here: the evictor is about to
		// take the room, so restoring then would fight that load.
		go r.restorePreloads()
		return nil
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
	go r.restorePreloads()
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
	// A swept model frees its card: an evicted preload may fit again. This
	// also retries a preload that had no room at the last free.
	go r.restorePreloads()
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
		return
	}
	// A preload model evicted to make room comes back when the room returns.
	r.notePreloadEvicted(instanceModel(id), forKey)
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
	// A pinned stanza is never evicted, for any reason. Its VRAM is not
	// reclaimable, so an admission that would need it fails instead of
	// silently pulling the model out from under a running service.
	if s := r.cfg().Stanza(id); s != nil && s.Pin {
		return false
	}
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
		p := r.peerByName(ref.Peer)
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

// resolveMesh picks the first peer rankPeers offers for modelID. Local
// stanzas are resolved before this runs.
func (r *Router) resolveMesh(modelID string) (Target, error) {
	ranked, _ := r.rankPeers(modelID, false)
	if len(ranked) == 0 {
		return Target{}, fmt.Errorf("no node can serve %q", modelID)
	}
	return ranked[0], nil
}

// rankPeers orders the peers that advertise modelID. A peer with the model
// loaded and a slot free (or an openai peer) comes first, then one that can
// admit a fresh load, then one that must wait or evict (that node queues it),
// then a loaded peer whose slots are full. Each tier keeps config order.
// When the local stanza enhances prompts, peers whose copy advertises the same
// caption format are ranked before the rest, which are only a fallback.
// slotSpill drops the loaded-but-full tier so the caller can queue locally.
// A router peer inside a remembered Retry-After is left out; soonest is the
// shortest such window still to run, 0 if none was left out.
func (r *Router) rankPeers(modelID string, slotSpill bool) (ranked []Target, soonest time.Duration) {
	type rankedPeer struct {
		target Target
		match  bool
	}
	var ready, admit, wait, full []rankedPeer
	wantCaption, allowFallback := r.localImageCaption(modelID)
	for _, p := range r.PeerList() {
		if !r.peerHasModel(p, modelID) {
			continue
		}
		match := wantCaption == "" || r.peerMatchesImagePipeline(p.Name, modelID, wantCaption)
		if !match && !allowFallback {
			continue
		}
		t := rankedPeer{target: Target{Peer: p.Name, PeerID: modelID}, match: match}
		if p.Kind == "openai" {
			ready = append(ready, t)
			continue
		}
		if rem := r.peerCooling(p.Name, modelID); rem > 0 {
			if soonest == 0 || rem < soonest {
				soonest = rem
			}
			continue
		}
		if !r.peers.Fresh(p.Name) {
			continue
		}
		loaded, free, known := r.peerSlotInfo(p.Name, modelID)
		switch {
		case known && loaded && free:
			ready = append(ready, t)
		case known && loaded:
			if !slotSpill {
				full = append(full, t)
			}
		case r.peerEligible(p.Name, modelID):
			admit = append(admit, t)
		default:
			// Card is occupied (in-flight, or our local vram_mb is larger than
			// their free bytes). They listed this model_id — POST /_router/load
			// and let that node wait/evict instead of 503 here.
			wait = append(wait, t)
		}
	}
	all := append(append(append(ready, admit...), wait...), full...)
	if wantCaption != "" {
		sort.SliceStable(all, func(i, j int) bool { return all[i].match && !all[j].match })
	}
	ranked = make([]Target, len(all))
	for i := range all {
		ranked[i] = all[i].target
	}
	return ranked, soonest
}

// peerCooling returns how long peer must still be left alone for model, from
// the Retry-After it sent. The window is the peer's; the origin keeps no
// doubling clock of its own.
func (r *Router) peerCooling(peer, model string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Until(r.peerUntil[peer+"/"+model])
}

// notePeerLoad remembers the window a peer sent for model, or forgets any
// earlier one when it sent none or accepted the load.
func (r *Router) notePeerLoad(peer, model string, window time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if window > 0 {
		r.peerUntil[peer+"/"+model] = time.Now().Add(window)
	} else {
		delete(r.peerUntil, peer+"/"+model)
	}
}

// peerRefusal is a peer's refusal to load a model, with the Retry-After it
// sent (0 if none).
type peerRefusal struct {
	msg        string
	retryAfter time.Duration
}

func (e *peerRefusal) Error() string { return e.msg }

// parseRetryAfter reads delay-seconds or an HTTP date; anything else is 0.
func parseRetryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// peerModels is the models a peer serves. A discovered peer's scan-time Models
// go stale between scans; the syncer refreshes its cache every few seconds,
// so read the catalog there. A configured router peer serves the model list
// the operator wrote plus the catalog its telemetry advertises; an openai peer
// or an older router that sends no catalog has only the written list.
func (r *Router) peerModels(p *config.Peer) []string {
	t, _, _ := r.peers.Entry(p.Name)
	if t == nil {
		return p.Models
	}
	if r.cfg().Peer(p.Name) == nil {
		if models := modelsFrom(t); len(models) > 0 {
			return models
		}
		return p.Models
	}
	out := slices.Clone(p.Models)
	for _, m := range t.Catalog {
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// meshAdvertises reports whether any configured or discovered peer serves id.
func (r *Router) meshAdvertises(id string) bool {
	return slices.ContainsFunc(r.PeerList(), func(p *config.Peer) bool { return r.peerHasModel(p, id) })
}

// refuseUnshared answers 404 when a peer asks for a local model kept off the
// mesh (share: false), even if the caller's config still names it.
func (r *Router) refuseUnshared(w http.ResponseWriter, req *http.Request, id string) bool {
	if r.fromPeer(req) == "" {
		return false
	}
	cfg := r.cfg()
	s := cfg.Stanza(id)
	if s == nil {
		s = cfg.Alias(id)
	}
	if s == nil || s.Shared() {
		return false
	}
	http.Error(w, "model not shared", http.StatusNotFound)
	return true
}

func (r *Router) peerHasModel(p *config.Peer, modelID string) bool {
	return slices.Contains(r.peerModels(p), modelID)
}

// localImageCaption is the local stanza's caption format, empty when it has no
// caption step, and whether a peer that lacks it may still take the request.
func (r *Router) localImageCaption(modelID string) (caption string, allowFallback bool) {
	s := r.stanzaOf(modelID)
	if s == nil || s.ImagePipeline == nil {
		return "", true
	}
	return s.ImagePipeline.Caption, imagePipelineFallback(s)
}

// imagePipelineFallback is whether a peer whose copy advertises no matching
// image pipeline may still take a request. It is tried after every matching
// peer, only when none of them can serve it.
//
// shortcut: always allow the fallback, gate it on the per-stanza peer_spill
// policy once that option lands.
func imagePipelineFallback(*config.Stanza) bool { return true }

// peerImageCaption is the caption format peer advertises for modelID and
// whether its telemetry reports an image pipeline at all. Older peers omit the
// map. The map covers configured stanzas, so a copy that has not loaded the
// model still reports how it would serve it.
func (r *Router) peerImageCaption(name, modelID string) (caption string, reported bool) {
	t := r.peers.Snapshot()[name]
	if t == nil {
		return "", false
	}
	if p, ok := t.Models[modelID]; ok && p != nil {
		return p.Caption, true
	}
	return "", false
}

// peerMatchesImagePipeline reports whether peer's copy of modelID advertises
// the same caption format.
func (r *Router) peerMatchesImagePipeline(name, modelID, caption string) bool {
	got, reported := r.peerImageCaption(name, modelID)
	return reported && got == caption
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
			if o := r.stanzaOf(id); o != nil && o.Pin {
				continue // never evicted; canEventuallyAdmit counts its room as gone
			}
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
	p := r.peerByName(name)
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
	pool := r.cfg().Pool(poolName)
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
	cfg := r.cfg()
	for _, target := range pool.Targets {
		if skip[target] {
			continue
		}
		ref := resolveRef(target, cfg)
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
				limit = cfg.Stanza(ref.Local).SlotCount()
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
			if r.localCanServe(r.cfg().Stanza(ref.Local)) {
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
	cs, req := withCleanups(req)
	defer cs.run()
	if _, id := jobPath(req.URL.Path); id != "" {
		r.serveJob(w, req, id)
		return
	}
	if r.cfg().VoiceDir != "" {
		library, answered := r.voiceLibraryRoute(w, req)
		if answered {
			return
		}
		if library {
			r.serveVoiceLibrary(w, req)
			return
		}
	}
	if isVoiceList(req) && r.serveVoiceUnion(w, req) {
		return
	}
	if isWebsocketUpgrade(req) && pathAPIType(req.URL.Path) == config.APITypeRealtime && req.URL.Query().Get("model") == "" {
		if u := r.answerUpgrade(w, req); u != nil {
			defer u.finish()
			if u.gone {
				return
			}
			w = u
		}
	}
	ref, err := matchRequest(req, r.cfg(), r.meshAdvertises)
	if err != nil {
		if errors.Is(err, ErrNoModel) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		var amb *ambiguousTypeError
		if errors.As(err, &amb) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.writeBodyError(w, req, err)
		return
	}
	req = withRequestedID(req, ref.Raw)
	if ref.Pool != "" {
		r.servePool(w, req, ref.Pool)
		return
	}
	if ref.Local != "" {
		r.serveLocalTarget(w, req, ref.Local)
		return
	}
	if ref.Peer != "" {
		r.servePeers(w, req, ref.Peer, ref.PeerID)
		return
	}
	if ref.Raw != "" {
		// Mesh id only: peers advertise it, and it is not a local stanza,
		// pool, or peer-qualified name. Raw is also filled for those
		// other kinds, so this branch must run last.
		r.servePeers(w, req, "", ref.Raw)
		return
	}
	http.Error(w, "no router for requested model", http.StatusNotFound)
}

func (r *Router) serveLocalTarget(w http.ResponseWriter, req *http.Request, modelID string) {
	if r.refuseUnshared(w, req, modelID) {
		return
	}
	incomingPeer := req.Header.Get("X-Model-Router-Peer") != ""
	s := r.cfg().Stanza(modelID)
	// A bad envelope is refused before it can cold-start the model or spill.
	if isSystemOne(req, s) {
		if err := checkSystemOneRequest(req, modelID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	// One admit budget for the whole local path: a sibling retry after the
	// first slot wait times out must not start a second full wait.
	if s != nil && s.APIType == config.APITypeSpeech && isSynthesis(req) {
		if _, ok := req.Context().Value(admitDeadlineKey{}).(time.Time); !ok {
			req = req.WithContext(context.WithValue(req.Context(), admitDeadlineKey{}, time.Now().Add(r.cfg().AdmitTimeout())))
		}
	}
	// auto offers a blocked request to a peer at once. on_failure and never
	// skip both early spills and wait for local admission instead.
	earlySpill := !incomingPeer && s != nil && peerSpillAuto(s)
	// If this GPU cannot admit now, try an idle peer before waiting locally.
	if earlySpill && !r.localCanServe(s) && r.spillWhileBlocked(w, req, s) {
		return
	}
	// A free slot on a running instance, or a new instance on a free GPU,
	// beats sending the request off-node.
	if !incomingPeer && s != nil {
		if key, ok := r.acquireInstance(s, false); ok {
			err := r.proxyLocalHeld(w, req, key)
			r.releaseOccupancy(Target{Local: key})
			if err == nil {
				return
			}
			if r.hasLiveSibling(key) && r.proxyLocal(w, req, modelID) == nil {
				return
			}
			if r.serveSpillAfterLocalFailure(w, req, modelID, s, err) {
				return
			}
			loadFailed(w, err)
			return
		}
	}
	if earlySpill && r.serveSlotSpill(w, req, modelID) {
		return
	}
	if err := r.proxyLocal(w, req, modelID); err != nil {
		if !incomingPeer && r.serveSpillAfterLocalFailure(w, req, modelID, s, err) {
			return
		}
		loadFailed(w, err)
	}
}

// peerSpillAuto reports whether s spills before trying local admission. An
// unset field is auto (config.Parse sets it, but tests build stanzas directly).
func peerSpillAuto(s *config.Stanza) bool {
	return s == nil || s.PeerSpill == "" || s.PeerSpill == config.PeerSpillAuto
}

// serveSpillAfterLocalFailure walks to a peer after a local start or proxy
// error, when s's peer_spill allows it. auto walks on any failure (today's
// behaviour). on_failure walks only when the local copy cannot serve here: a
// hard start failure, an open start cooldown, or an admit timeout. never also
// skips the hard-failure walk, so the client gets this node's 503; an admit
// timeout still walks.
func (r *Router) serveSpillAfterLocalFailure(w http.ResponseWriter, req *http.Request, modelID string, s *config.Stanza, err error) bool {
	if !peerSpillAuto(s) {
		switch s.PeerSpill {
		case config.PeerSpillOnFailure:
			if !localStartCannotServe(err) {
				return false
			}
		case config.PeerSpillNever:
			var ae *admitError
			if !errors.As(err, &ae) {
				return false
			}
		}
	}
	return r.serveSpill(w, req, modelID)
}

// localStartCannotServe reports whether err means this node cannot run the
// model here within the admit budget: the child died or timed out, its start
// cooldown is open, or no GPU room came free in time.
func localStartCannotServe(err error) bool {
	var cd *startCooldownError
	var ae *admitError
	return hardStartFailure(err) || errors.As(err, &cd) || errors.As(err, &ae)
}

// admitDeadlineKey carries the end of a request's admit budget in its context,
// so time spent waiting on peers is not given again to the local admission wait.
type admitDeadlineKey struct{}

// noLoadKey marks an in-process router request that may only use a model that
// is already running. The serving path loads a stopped local model on demand
// otherwise.
type noLoadKey struct{}

// spillRetryInterval is how often a request that local VRAM cannot take asks
// the peers again. A var so tests can shorten it.
var spillRetryInterval = 3 * time.Second

// spillWhileBlocked keeps offering req to peers until one serves it, local
// room opens (false: carry on locally), the client goes, or the admit timeout
// ends (503 with Retry-After). True means the response is written.
func (r *Router) spillWhileBlocked(w http.ResponseWriter, req *http.Request, s *config.Stanza) bool {
	snap, err := r.snapshotBody(req, r.cfg().RequestBodyLimit(s.ModelID))
	if err != nil {
		r.writeBodyError(w, req, err)
		return true
	}
	// No deferred cleanup here: on false the local path still reads the body,
	// so a spooled one is removed by the request's cleanup hook (snapshotBody
	// registers it) when ServeHTTP ends.
	// Every attempt, and the local path after it, starts from the original
	// body: an earlier attempt may have read it or rewritten its model field.
	// install only fails if the spool vanished; the request then carries no body.
	resetBody := func() { _ = snap.install(req) }
	deadline := time.Now().Add(r.cfg().AdmitTimeout())
	// One admit budget for the peer walk and the local admit that follows:
	// peer loads and the local wait both read it from the request.
	*req = *req.WithContext(context.WithValue(req.Context(), admitDeadlineKey{}, deadline))
	var askedPeers time.Time
	for {
		if time.Since(askedPeers) >= spillRetryInterval {
			askedPeers = time.Now()
			resetBody()
			if r.serveSpill(w, req, s.ModelID) {
				return true
			}
		}
		resetBody()
		if r.localCanServe(s) {
			return false
		}
		if time.Now().After(deadline) {
			r.logger.Warnf("no room for %s here and no peer took it; giving up", s.ModelID)
			loadFailed(w, &admitError{fmt.Sprintf("no GPU can take %s now and no peer served it", s.ModelID)})
			return true
		}
		// Wake on the occupancy poke (or the 200 ms fallback) like waitAdmit,
		// so room that opens locally is taken at once rather than on the next
		// poll; the peers are still asked again every spillRetryInterval.
		if err := r.waitAdmit(req.Context()); err != nil {
			return true
		}
	}
}

func (r *Router) servePool(w http.ResponseWriter, req *http.Request, poolName string) {
	pool := r.cfg().Pool(poolName)
	if pool == nil {
		http.Error(w, fmt.Sprintf("unknown pool %q", poolName), http.StatusServiceUnavailable)
		return
	}
	snap, err := r.snapshotBody(req, r.cfg().RequestBodyLimit(""))
	if err != nil {
		r.writeBodyError(w, req, err)
		return
	}
	defer snap.cleanup()
	skip := map[string]bool{}
	// A peer never gets a share: false member, even through a pool name.
	if r.fromPeer(req) != "" {
		cfg := r.cfg()
		unshared := 0
		for _, t := range pool.Targets {
			if id := resolveRef(t, cfg).Local; id != "" && !cfg.Stanza(id).Shared() {
				skip[t] = true
				unshared++
			}
		}
		if unshared == len(pool.Targets) {
			http.Error(w, "model not shared", http.StatusNotFound)
			return
		}
	}
	var last error
	for {
		tgt, err := r.pickPoolTarget(pool, skip)
		if err != nil {
			if last != nil {
				loadFailed(w, last)
				return
			}
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		skip[occupancyKey(tgt)] = true
		// Each attempt starts from the original body; the last one consumed it.
		_ = snap.install(req)
		aw := &attemptWriter{dst: w, h: http.Header{}}
		var serveErr error
		switch {
		case tgt.Local != "":
			serveErr = r.proxyLocalHeld(aw, req, tgt.Local)
		case tgt.Peer != "":
			serveErr = r.proxyPeer(aw, req, tgt.Peer, tgt.PeerID)
		default:
			serveErr = fmt.Errorf("no router for requested model")
		}
		r.releaseOccupancy(tgt)
		if serveErr == nil && aw.failed != 0 {
			serveErr = fmt.Errorf("upstream %s answered %d", occupancyKey(tgt), aw.failed)
		}
		if serveErr == nil {
			return
		}
		last = serveErr
		r.logger.Warnf("pool %s: %s failed before any response (%v); trying next target", poolName, occupancyKey(tgt), serveErr)
		r.logger.Meshf("pool %s: skipped %s: %v", poolName, occupancyKey(tgt), serveErr)
	}
}

// attemptWriter holds one pool attempt's response head back from the client.
// A 5xx head is swallowed with its body and recorded in failed, so servePool can
// try the next target; any other head passes through and commits the attempt.
// Once committed (started) nothing is retried.
type attemptWriter struct {
	dst     http.ResponseWriter
	h       http.Header
	started bool
	failed  int
}

func (a *attemptWriter) Header() http.Header { return a.h }

func (a *attemptWriter) WriteHeader(code int) {
	if a.started || a.failed != 0 {
		return
	}
	if code >= 500 {
		a.failed = code
		return
	}
	for k, v := range a.h {
		a.dst.Header()[k] = v
	}
	a.dst.WriteHeader(code)
	a.started = true
}

func (a *attemptWriter) Write(b []byte) (int, error) {
	if !a.started && a.failed == 0 {
		a.WriteHeader(http.StatusOK)
	}
	if a.failed != 0 {
		return len(b), nil
	}
	return a.dst.Write(b)
}

// Hijack commits the attempt: a websocket session cannot be retried on the
// next target once the backend has switched protocols.
func (a *attemptWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	a.started = true
	return http.NewResponseController(a.dst).Hijack()
}

func (a *attemptWriter) Flush() {
	if a.started {
		_ = http.NewResponseController(a.dst).Flush()
	}
}

// serveSpill tries later pool targets after a direct local id could not start,
// then any peer that advertises the same model_id. Incoming peer-proxied
// requests do not bounce (X-Model-Router-Peer).
func (r *Router) serveSpill(w http.ResponseWriter, req *http.Request, failedLocal string) bool {
	_, noLoad := req.Context().Value(noLoadKey{}).(bool)
	for _, raw := range r.spillTargets(failedLocal) {
		ref := resolveRef(raw, r.cfg())
		switch {
		case ref.Local != "" && ref.Local != failedLocal:
			if !r.localCanServe(r.cfg().Stanza(ref.Local)) {
				continue
			}
			if err := r.proxyLocal(w, req, ref.Local); err == nil {
				return true
			}
		case ref.Peer != "":
			if noLoad && (r.peerIsOpenAI(ref.Peer) || !r.peerLoads(ref.Peer, ref.PeerID)) {
				continue
			}
			if !r.peerEligible(ref.Peer, ref.PeerID) {
				continue
			}
			if err := r.proxyPeer(w, req, ref.Peer, ref.PeerID); err == nil {
				return true
			}
		}
	}
	served, _, _ := r.walkPeers(w, req, "", failedLocal, false)
	return served
}

// serveSlotSpill sends a request to a peer that already has a free slot (or
// can admit a fresh load) when local occupancy is at --parallel. Incoming
// peer-proxied requests do not bounce. If nothing can take it, the caller
// queues on the local backend.
func (r *Router) serveSlotSpill(w http.ResponseWriter, req *http.Request, localID string) bool {
	if req.Header.Get("X-Model-Router-Peer") != "" {
		return false
	}
	served, _, _ := r.walkPeers(w, req, "", localID, true)
	return served
}

// servePeers serves a request that has no local model: the named peer first
// (empty for a mesh id), then the other router peers. When every peer refuses
// the client gets 503 with the last failure and the shortest Retry-After sent.
func (r *Router) servePeers(w http.ResponseWriter, req *http.Request, first, modelID string) {
	served, last, retry := r.walkPeers(w, req, first, modelID, false)
	if served {
		return
	}
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((retry+time.Second-1)/time.Second)))
	}
	http.Error(w, last.Error(), http.StatusServiceUnavailable)
}

// walkPeers tries first (if set), then the peers rankPeers offers, until one
// writes a response. A refused load wrote nothing, so the next peer is tried;
// once a peer has written bytes the walk is over. An openai peer is a single
// hop: a failure there ends the walk. On failure it returns the last error and
// the shortest Retry-After any refusal sent (0 if none sent one).
func (r *Router) walkPeers(w http.ResponseWriter, req *http.Request, first, modelID string, slotSpill bool) (served bool, last error, retry time.Duration) {
	ranked, soonest := r.rankPeers(modelID, slotSpill)
	if first != "" {
		rest := ranked[:0:0]
		for _, c := range ranked {
			if c.Peer != first {
				rest = append(rest, c)
			}
		}
		ranked = rest
		if rem := r.peerCooling(first, modelID); rem > 0 && !r.peerIsOpenAI(first) {
			if soonest == 0 || rem < soonest {
				soonest = rem
			}
		} else {
			ranked = append([]Target{{Peer: first, PeerID: modelID}}, ranked...)
		}
	}
	// A no-load request may only reach a peer that already serves the model:
	// a paid upstream would receive the prompt, and a router peer that only
	// lists the model would be asked to cold-load it.
	if _, noLoad := req.Context().Value(noLoadKey{}).(bool); noLoad {
		kept := ranked[:0]
		for _, t := range ranked {
			if !r.peerIsOpenAI(t.Peer) && r.peerLoads(t.Peer, modelID) {
				kept = append(kept, t)
			}
		}
		ranked = kept
	}
	wantCaption, _ := r.localImageCaption(modelID)
	for _, t := range ranked {
		note := ""
		if wantCaption != "" && t.Peer != first && !r.peerMatchesImagePipeline(t.Peer, modelID, wantCaption) {
			note = " (fallback: no " + wantCaption + " caption)"
		}
		r.logger.Meshf("peer walk %s -> %s%s", modelID, t.Peer, note)
		err := r.proxyPeer(w, req, t.Peer, t.PeerID)
		if err == nil {
			return true, nil, 0
		}
		last = err
		var ref *peerRefusal
		if errors.As(err, &ref) && ref.retryAfter > 0 && (retry == 0 || ref.retryAfter < retry) {
			retry = ref.retryAfter
		}
		if r.peerIsOpenAI(t.Peer) {
			break
		}
	}
	if last == nil {
		// Nothing was asked: every router peer is inside a window it sent.
		last, retry = fmt.Errorf("no node can serve %q", modelID), soonest
		r.logger.Meshf("peer walk %s: no peer chosen (%s)", modelID, r.noPeerReason(modelID))
	}
	return false, last, retry
}

// noPeerReason says why each peer was left out of a walk.
func (r *Router) noPeerReason(modelID string) string {
	var out []string
	for _, p := range r.PeerList() {
		switch rem := r.peerCooling(p.Name, modelID); {
		case !r.peerHasModel(p, modelID):
			out = append(out, p.Name+": does not advertise it")
		case p.Kind != "openai" && rem > 0:
			out = append(out, fmt.Sprintf("%s: cooling for %ds", p.Name, int(rem.Seconds())))
		case p.Kind != "openai" && !r.peers.Fresh(p.Name):
			out = append(out, p.Name+": telemetry stale")
		default:
			out = append(out, p.Name+": refused")
		}
	}
	if len(out) == 0 {
		return "no peers configured"
	}
	return strings.Join(out, ", ")
}

func (r *Router) spillTargets(failedLocal string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range r.cfg().Pools {
		found := false
		for _, t := range p.Targets {
			ref := resolveRef(t, r.cfg())
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
		loadFailed(w, err)
	}
}

// proxyLocal serves req on the best instance of modelID, queueing on the
// least-loaded one when every instance is busy.
func (r *Router) proxyLocal(w http.ResponseWriter, req *http.Request, modelID string) error {
	s := r.cfg().Stanza(modelID)
	if s == nil {
		return fmt.Errorf("unknown local model %q", modelID)
	}
	var key string
	if s.APIType == config.APITypeTranscription {
		var err error
		if key, err = r.waitFreeSlot(req.Context(), s); err != nil {
			return err
		}
	} else {
		var ok bool
		if key, ok = r.acquireInstance(s, true); !ok {
			return fmt.Errorf("router shutting down")
		}
	}
	defer r.releaseOccupancy(Target{Local: key})
	return r.proxyLocalHeld(w, req, key)
}

// backendPathKey carries a request's path after the stanza's path_rewrite.
type backendPathKey struct{}

// waitFreeSlot takes a slot on s without overbooking it, waiting up to the
// request's admit budget. A transcription backend runs one upload at a time
// and holds each one it has accepted in memory, so extra uploads wait here,
// unread, rather than queue on the backend. Past the budget it is a 503 with
// Retry-After, like any other admit wait.
func (r *Router) waitFreeSlot(ctx context.Context, s *config.Stanza) (string, error) {
	deadline := time.Now().Add(r.cfg().AdmitTimeout())
	if d, ok := ctx.Value(admitDeadlineKey{}).(time.Time); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		if key, ok := r.acquireInstance(s, false); ok {
			return key, nil
		}
		if time.Now().After(deadline) {
			return "", &admitError{fmt.Sprintf("every slot of %s is busy", s.ModelID)}
		}
		if err := r.waitAdmit(ctx); err != nil {
			return "", err
		}
	}
}

// proxyLocalHeld loads instance key if needed and proxies to it. The caller
// already holds an occupancy slot on key.
func (r *Router) proxyLocalHeld(w http.ResponseWriter, req *http.Request, key string) error {
	if r.cfg().StreamLoadProgress && wantsStream(req) {
		lw := &loadStream{ResponseWriter: w}
		err := r.loadReportingProgress(lw, req, key)
		if err != nil && lw.started {
			// The 200 is already out, so a spill or http.Error cannot follow.
			lw.fail(err)
			return nil
		}
		if err != nil {
			return err
		}
		w = lw
	} else if _, err := r.load(req.Context(), key, true, true); err != nil {
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
	if s := r.stanzaOf(key); s != nil {
		if s.APIType == config.APITypeSpeech && isSynthesis(req) {
			release, err := r.takeSpeechSlot(req.Context(), key, s.SlotCount())
			if err != nil {
				return err
			}
			defer release()
		}
		// An img2img body may carry an `outpaint` field, and that rewrite must
		// reach every image stanza, not only the ones with an image_pipeline
		// (#200). Other requests keep the plain proxy path.
		if s.ImagePipeline != nil || (s.APIType == config.APITypeImage && req.URL.Path == "/sdapi/v1/img2img") {
			r.proxyImage(w, req, Target{Local: key}, m, m.proxy(), key, r.node, s)
			return nil
		}
		if s.APIType == config.APITypeSpeech && isSynthesis(req) {
			if err := r.injectLibraryVoice(req, s); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return nil
			}
		}
		if err := filterLocal(req, s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return nil
		}
		if s.APIType == config.APITypeTranscription {
			w = &transcriptionWriter{ResponseWriter: w}
		}
		// The rewrite rides on the context so activity and metrics keep the
		// client's path; only proxyTo's Director sends the backend's.
		if p := s.BackendPath(req.URL.Path); p != req.URL.Path {
			req = req.WithContext(context.WithValue(req.Context(), backendPathKey{}, p))
		}
		if s.RequestTimeoutSeconds > 0 {
			ctx, cancel := context.WithTimeout(req.Context(), time.Duration(s.RequestTimeoutSeconds)*time.Second)
			defer cancel()
			req = req.WithContext(ctx)
		}
		if isSystemOne(req, s) {
			req = req.WithContext(context.WithValue(req.Context(), decisionReplyKey{}, s.ModelID))
		}
	}
	r.proxyJobAware(w, req, Target{Local: key}, m.proxy(), key, r.node)
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
	p := r.peerByName(peerName)
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
	// Resolve a library voice on the way out, so a peer needs no copy of it.
	if isSynthesis(req) {
		if err := r.injectLibraryVoice(req, r.speechStanzaFor(peerModel)); err != nil {
			return err
		}
	}
	// The request may still name the model as "peerName/peerModel"; the peer
	// only knows its local id, so rewrite the model field before proxying
	// (llama-swap's ReplaceRequestModel does the same). The field rewritten is
	// the peer's configured body_field, not a hardcoded "model".
	if err := rewriteBodyModel(req, peerModel, p.BodyField, p.Filters); err != nil {
		return err
	}
	req.Header.Set("X-Model-Router-Peer", r.node)
	r.logger.Meshf("%s %s -> %s (%s)", req.Method, req.URL.Path, peerName, peerModel)
	r.proxyJobAware(w, req, Target{Peer: peerName, PeerID: peerModel}, p.BaseURL, peerModel, peerName)
	return nil
}

// rewriteBodyModel replaces the model field in a request body (or query) with
// the given id, restoring the body for downstream use. JSON bodies, and
// urlencoded or multipart forms, are rewritten to field (the peer's configured
// body_field), preserving all other fields and file parts. A "model" query
// param, which body_field does not cover, is rewritten on any method: the
// peer's matcher reads it on GETs and on multipart uploads. The peer's filters
// f are applied too: a form body takes both in one pass.
func rewriteBodyModel(req *http.Request, newModel, field string, f config.Filters) error {
	q := req.URL.Query()
	if q.Get("model") != "" {
		q.Set("model", newModel)
		req.URL.RawQuery = q.Encode()
	}
	ct := req.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		// editForm ignores non-form bodies itself; naming the form types here
		// keeps the pass-through decision visible at the call site, where a
		// reviewer or reader looks for it (review round, PR #107).
		isForm := strings.Contains(ct, "multipart/form-data") ||
			strings.Contains(ct, "application/x-www-form-urlencoded")
		if !isForm {
			return nil
		}
		e, err := newFormEdit(f.RenameParams, f.StripParams, f.SetParams)
		if err != nil {
			return err
		}
		e.set[field] = &formSet{force: &newModel} // config forbids filters on body_field, so nothing clashes
		return editForm(req, e)
	}
	if err := rewriteJSONModel(req, newModel, field); err != nil {
		return err
	}
	return applyFilters(req, f.RenameParams, f.StripParams, f.SetParams)
}

// rewriteJSONModel sets field to newModel on a JSON object body; any other
// body is forwarded unchanged.
func rewriteJSONModel(req *http.Request, newModel, field string) error {
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
	// A request already waiting on peers keeps one admit budget: its load
	// call must not outlive it.
	if d, ok := ctx.Value(admitDeadlineKey{}).(time.Time); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, d)
		defer cancel()
	}
	body, _ := json.Marshal(map[string]string{"model_id": modelID})
	url := p.BaseURL + "/_router/load"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("peer %s load: %w", p.Name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Model-Router-Peer", r.node)
	resp, err := r.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("peer %s load: %w", p.Name, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		window := parseRetryAfter(resp.Header.Get("Retry-After"))
		r.notePeerLoad(p.Name, modelID, window)
		return &peerRefusal{
			msg:        fmt.Sprintf("peer %s rejected load of %s (%d)", p.Name, modelID, resp.StatusCode),
			retryAfter: window,
		}
	}
	r.notePeerLoad(p.Name, modelID, 0)
	return nil
}

// originMatchesHost reports whether an Origin header addresses host. Only
// that origin (plus a request with no Origin) may have its site-context
// headers stripped; a malformed or opaque "null" origin never matches.
func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, host)
}

// transcriptionWriter gives a text transcript the type the OpenAI API
// specifies. whisper-server answers text as text/html and srt as
// application/x-subrip, which clients and browsers would treat as markup.
type transcriptionWriter struct{ http.ResponseWriter }

func (w *transcriptionWriter) WriteHeader(code int) {
	if code >= 200 && code < 300 {
		ct, _, _ := strings.Cut(w.Header().Get("Content-Type"), ";")
		switch strings.TrimSpace(ct) {
		case "text/html", "application/x-subrip", "text/vtt":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *transcriptionWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush keeps a streamed transcript flushing through the wrappers that test
// for http.Flusher.
func (w *transcriptionWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// beginProxy starts the in-flight metric and Activity capture for one client
// request. A caller that retries holds one pair for the whole exchange, so the
// request is counted and logged once. done is always safe to call.
func (r *Router) beginProxy(w http.ResponseWriter, req *http.Request, model, target string) (http.ResponseWriter, func()) {
	var sw *sessionWriter
	if isWebsocketUpgrade(req) {
		sw = &sessionWriter{ResponseWriter: w, r: r, model: instanceModel(model)}
		w = sw
	}
	wrapped, done := r.wrapProxy(w, req, model, target)
	finish := func() {
		if r.metrics != nil {
			r.metrics.DecInflight()
		}
		done()
		if sw != nil {
			sw.end()
		}
	}
	if r.metrics != nil {
		r.metrics.IncInflight()
	}
	return wrapped, finish
}

// proxyTo reverse-proxies the request to baseURL, preserving path and body and
// streaming SSE responses. Buffering is disabled for streaming.
func (r *Router) proxyTo(w http.ResponseWriter, req *http.Request, baseURL, model, target string) {
	wrapped, done := r.beginProxy(w, req, model, target)
	defer done()
	r.proxyToWriter(wrapped, req, baseURL, model, target)
}

// proxyToWriter is proxyTo without the per-request metrics and Activity
// capture; its caller owns one beginProxy pair for the whole exchange.
func (r *Router) proxyToWriter(w http.ResponseWriter, req *http.Request, baseURL, model, target string) {
	u, err := url.Parse(baseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// The matcher left r.Body on the prepared request body: in-memory JSON, a
	// stream (prefix plus the rest of the client body), or a temp file. The
	// ReverseProxy streams it to the backend without another full copy.
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			r.Host = u.Host
			// Keep the original path (peer router / backend routes on it),
			// unless the stanza's path_rewrite gave the backend's own.
			r.URL.Path = req.URL.Path
			if p, ok := req.Context().Value(backendPathKey{}).(string); ok {
				r.URL.Path, r.URL.RawPath = p, ""
			}
			r.URL.RawQuery = req.URL.RawQuery
			// This router is the browser trust boundary. It vouches only for a
			// request with no Origin (a non-browser client) or one from its own
			// /ui/ page (Origin host equals the request host). Those lose the
			// site-context headers, so a backend or peer does not read the
			// proxied call as cross-site. Any other Origin stays in place, so a
			// backend still refuses a foreign page. r is ReverseProxy's clone,
			// so the Activity capture records the headers as received.
			if origin := r.Header.Get("Origin"); origin == "" || originMatchesHost(origin, req.Host) {
				for h := range r.Header {
					if h == "Origin" || h == "Referer" || strings.HasPrefix(h, "Sec-Fetch-") {
						r.Header.Del(h)
					}
				}
			}
		},
		// Flush every chunk: SSE, streamed TTS audio and other media start
		// reaching the client before the backend ends.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A chunked upload ran past the cap mid-stream (limitBody): the
			// backend request was cut off, so the client gets 413, not 502.
			if errors.Is(err, errBodyTooLarge) {
				http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
				return
			}
			if errors.Is(err, errBadMultipart) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				http.Error(w, "upstream timed out: "+err.Error(), http.StatusGatewayTimeout)
				return
			}
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
	if model, ok := req.Context().Value(decisionReplyKey{}).(string); ok {
		proxy.ModifyResponse = checkDecisionReply(model)
	}
	// Tell any fronting proxy (nginx on gareth) not to buffer SSE.
	w.Header().Set("X-Accel-Buffering", "no")
	proxy.ServeHTTP(w, req)
}

// peerIsOpenAI is false for an unknown peer: a reload may have removed it
// since the caller picked it.
func (r *Router) peerIsOpenAI(name string) bool {
	p := r.peerByName(name)
	return p != nil && p.Kind == "openai"
}

// Preload loads the configured startup models in order.
func (r *Router) Preload() {
	cfg := r.cfg()
	for _, id := range cfg.Preload {
		if cfg.Stanza(id) == nil {
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
	// Capabilities lists what the backend supports (vision, tools, ...).
	// Always published, including an empty list, so a model with no flags is
	// not mistaken for "unknown, inherit the others".
	Capabilities []string `json:"capabilities"`
	// CloneMode is a speech stanza's voice reference shape (ref_audio or
	// speaker_ref_b64), so a client can send a one-off clone the backend takes.
	CloneMode string `json:"clone_mode,omitempty"`
	// GPUs lists where running local instances sit, one entry per instance.
	GPUs []int `json:"gpus,omitempty"`
	// Family is the model's family and maker; nil when unknown.
	Family *Family `json:"family,omitempty"`
	// Info is the owner, parameter count and per-type detail; nil when unknown.
	Info *ModelInfo `json:"info,omitempty"`
	// Peers lists every configured peer that serves this id, in config order.
	Peers []PeerModelStatus `json:"peers,omitempty"`
	// PathDefault marks the stanza a request lands on when it names no model,
	// so the UI can gate its mode controls on that stanza's capabilities (#200).
	PathDefault bool `json:"path_default,omitempty"`
}

// PeerModelStatus is one peer's copy of a catalog model.
type PeerModelStatus struct {
	Name     string `json:"name"`
	State    string `json:"state"` // loaded | available (advertised, not running) | stale
	GPUs     []int  `json:"gpus,omitempty"`
	InFlight int    `json:"in_flight,omitempty"`
	Slots    int    `json:"slots,omitempty"`
	LastSeen int64  `json:"last_seen,omitempty"` // unix seconds of the last telemetry received
}

// peerModelStatus reads one peer's state for model id from its cached
// telemetry. Stale telemetry says nothing about what is loaded now.
func peerModelStatus(p *config.Peer, id string, t *Telemetry, seen time.Time, fresh bool) PeerModelStatus {
	ps := PeerModelStatus{Name: p.Name, State: "available"}
	if !seen.IsZero() {
		ps.LastSeen = seen.Unix()
	}
	if p.Kind == "openai" {
		return ps // no telemetry to read; routable whenever configured
	}
	if !fresh {
		ps.State = "stale"
		return ps
	}
	for _, lm := range t.LoadedModels {
		if lm.ID != id {
			continue
		}
		ps.State = "loaded"
		gpus := lm.GPUs
		if len(gpus) == 0 && lm.GPU >= 0 {
			gpus = []int{lm.GPU} // older peers send only the first GPU
		}
		for _, g := range gpus {
			if !slices.Contains(ps.GPUs, g) {
				ps.GPUs = append(ps.GPUs, g)
			}
		}
		ps.InFlight += lm.InFlight
		ps.Slots += max(lm.Slots, 1) // as peerSlotInfo: no slot count means one
	}
	return ps
}

// publishedCaps returns caps for a catalog row. A nil slice becomes an empty
// one so JSON encodes [] rather than null.
func publishedCaps(c []string) []string {
	if c == nil {
		return []string{}
	}
	return c
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
	cfg := r.cfg()
	md := r.meta.Load()
	for _, id := range cfg.StanzaIDs() {
		s := cfg.Stanza(id)
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
			if g := m.gpuIndex(); g >= 0 {
				gpus = append(gpus, g) // a CPU stanza has no card, so "gpu: none"
			}
			if !m.IsStale(now) {
				freshness = "fresh"
			} else if freshness == "" {
				freshness = "stale"
			}
		}
		fam, info := r.catalogInfo(md, id, s)
		out = append(out, ModelStatus{
			ID: id, Name: s.Name, Type: "model", Origin: "local",
			State: string(st), Freshness: freshness, VramMB: s.VramMB, APIType: s.APIType,
			Capabilities: publishedCaps(s.Capabilities), CloneMode: s.CloneMode, GPUs: gpus, Family: fam, Info: info,
			PathDefault: s.Match.PathDefault,
		})
		seen[id] = true
		// A filter-set id is a catalog row of its own, sharing the stanza's state.
		for _, a := range s.FilterIDs() {
			row := out[len(out)-1]
			row.ID, row.VramMB = a, 0
			out = append(out, row)
			seen[a] = true
		}
		for _, a := range s.Aliases {
			seen[a] = true
		}
	}
	onPeers := map[string][]PeerModelStatus{}
	for _, p := range r.PeerList() {
		t, lastSeen, fresh := r.peers.Entry(p.Name)
		for _, m := range r.peerModels(p) {
			onPeers[m] = append(onPeers[m], peerModelStatus(p, m, t, lastSeen, fresh))
			// Only a routable peer adds a row of its own.
			if seen[m] || (p.Kind != "openai" && !fresh) {
				continue
			}
			seen[m] = true
			// A remote id has no stanza here; the metadata file and the peer's advertised hf_repo fill it in.
			fam, info := r.catalogInfo(md, m, remoteStanza(t, m))
			out = append(out, ModelStatus{ID: m, Type: "model", Origin: "remote", State: "available", Capabilities: publishedCaps(nil), Family: fam, Info: info})
		}
	}
	for i := range out {
		out[i].Peers = onPeers[out[i].ID]
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
	cfg := r.cfg()
	models := map[string]*ImagePipelineInfo{}
	for i := range cfg.Stanzas {
		s := &cfg.Stanzas[i]
		if s.ImagePipeline != nil && s.Shared() {
			models[s.ModelID] = &ImagePipelineInfo{Caption: s.ImagePipeline.Caption}
		}
	}
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
		if gpu >= 0 && item.inFlight == 0 {
			idleByGPU[gpu] += vram
		}
		slots := 1
		var gpus []int
		if gpu >= 0 {
			gpus = []int{gpu}
		}
		s := r.stanzaOf(item.id)
		if s != nil {
			slots = s.SlotCount()
			if splitPlacement(s) {
				gpus = stanzaGPUs(s)
			}
		}
		// An unshared model still counts toward the free figures above, but
		// peers are not told it runs here.
		if s != nil && !s.Shared() {
			continue
		}
		lm := LoadedModel{
			ID: instanceModel(item.id), Freshness: freshness, VramMB: vram, GPU: gpu, GPUs: gpus,
			Slots: slots, InFlight: item.inFlight,
		}
		if lm.ID != item.id {
			lm.Instance = item.id
		}
		loaded = append(loaded, lm)
	}
	// Every published free figure is after headroom, so peers never pick
	// this node for a load it must refuse.
	head := cfg.HeadroomMB
	for _, g := range gpus {
		stale := g.FreeMB + staleByGPU[g.Index]
		idle := g.FreeMB + idleByGPU[g.Index]
		if g.TotalMB > 0 {
			stale = min(stale, g.TotalMB)
			idle = min(idle, g.TotalMB)
		}
		g.FreeIfStaleEvictedMB = max(stale-head, 0)
		g.FreeIfIdleEvictedMB = max(idle-head, 0)
		g.FreeMB = max(g.FreeMB-head, 0)
	}
	t := &Telemetry{
		Node:         r.node,
		GPUs:         gpus,
		LoadedModels: loaded,
		Models:       models,
		Catalog:      r.localModelIDs(),
		HFRepos:      r.localHFRepos(),
		Timestamp:    now.Unix(),
	}
	if r.activity != nil {
		// Requests to an unshared model stay off the mesh too. InProgress and
		// Recent return fresh copies, so filtering them leaves the log (and the
		// local Activity API) with every row.
		unshared := func(model string) bool { s := r.stanzaOf(model); return s != nil && !s.Shared() }
		t.InProgress = slices.DeleteFunc(r.activity.InProgress(), func(e InProgressEntry) bool { return unshared(e.Model) })
		t.Recent = slices.DeleteFunc(r.activity.Recent(), func(e RecentEntry) bool { return unshared(e.Model) })
	}
	return t
}

// localModelIDs lists every shared local stanza id and filter-set alias.
func (r *Router) localModelIDs() []string {
	cfg := r.cfg()
	out := make([]string, 0, len(cfg.Stanzas))
	for i := range cfg.Stanzas {
		if !cfg.Stanzas[i].Shared() {
			continue
		}
		out = append(out, cfg.Stanzas[i].ModelID)
		out = append(out, cfg.Stanzas[i].FilterIDs()...)
	}
	return out
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
	cfg := r.cfg()
	s := cfg.Stanza(modelID)
	if s == nil {
		s = cfg.Alias(modelID)
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
	_, err := r.load(loadCtx, key, true, true)
	return err
}
