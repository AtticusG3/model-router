package router

import (
	"bytes"
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

	httpc *http.Client
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
		httpc:     &http.Client{Timeout: 0}, // no overall timeout; streaming is long
	}
}

// Ledger returns the admission ledger (used by the telemetry poller).
func (r *Router) Ledger() *Ledger { return r.ledger }

// Peers returns the peer cache (used by the peer syncer and status).
func (r *Router) Peers() *PeerCache { return r.peers }

// Load ensures the given local stanza is running. It runs the node's own
// admission control and returns the GPU index used.
func (r *Router) Load(modelID string) (int, error) {
	s := r.cfg.Stanza(modelID)
	if s == nil {
		return -1, fmt.Errorf("unknown local model %q", modelID)
	}

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
	if !ok {
		return -1, fmt.Errorf("no GPU has %d MB free for %s", s.VramMB, modelID)
	}

	if _, err := m.Start(); err != nil {
		r.ledger.Release(modelID)
		return -1, err
	}
	m.SetGPU(gpu)
	return gpu, nil
}

// Unload stops a local model and releases its VRAM reservation.
func (r *Router) Unload(modelID string) error {
	r.mu.Lock()
	m, ok := r.managed[modelID]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("not loaded: %s", modelID)
	}
	delete(r.managed, modelID)
	r.mu.Unlock()
	m.Stop()
	r.ledger.Release(modelID)
	return nil
}

// evictToFit unloads stale models until the ledger can admit s. Fresh and
// in-flight models are held: reload is expensive, unload is cheap, so idle
// backends stay until their VRAM is needed. gpu < 0 (auto device) considers
// every GPU's stale occupants.
func (r *Router) evictToFit(s *config.Stanza) {
	want := deviceIndex(s.Device)
	for _, id := range r.ledger.occupantsOnGPU(want, s.ModelID) {
		if r.busy(id) || !r.modelStale(id) {
			continue
		}
		r.logger.Infof("evicting stale %s to admit %s", id, s.ModelID)
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
// resolved before this runs. OpenAI peers are always eligible; router
// peers need fresh telemetry. Known VRAM is used as a hint; the peer
// still admits for real.
func (r *Router) resolveMesh(modelID string) (Target, error) {
	for i := range r.cfg.Peers {
		p := &r.cfg.Peers[i]
		if !r.peerHasModel(p, modelID) {
			continue
		}
		if p.Kind == "openai" {
			return Target{Peer: p.Name, PeerID: modelID}, nil
		}
		if !r.peerEligible(p.Name, modelID) {
			continue
		}
		return Target{Peer: p.Name, PeerID: modelID}, nil
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

func (r *Router) localCanServe(s *config.Stanza) bool {
	if s == nil {
		return false
	}
	var evict []string
	for _, id := range r.ledger.occupantsOnGPU(deviceIndex(s.Device), s.ModelID) {
		if !r.busy(id) {
			evict = append(evict, id)
		}
	}
	return r.ledger.CanAdmitEvicting(s, evict)
}

// peerEligible reports whether a peer is a spillover candidate. Known VRAM is
// a hint; unknown VRAM still tries a fresh peer and lets that node admit.
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
	vram := r.modelVram(modelID)
	if vram > 0 && !r.peerFits(name, vram) {
		return false
	}
	return true
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

// peerFits reports whether cached peer telemetry shows a GPU that can take
// vramMB now, or after that peer evicts its own stale models. Used only to
// pick a candidate; the peer admits for real. Unknown or zero vram (no local
// stanza/alias) fails closed.
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
		if g.FreeMB >= vramMB || g.FreeIfStaleEvictedMB >= vramMB {
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
	limit := pool.Spillover
	if limit <= 0 {
		limit = 1
	}

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
		if err := r.proxyLocal(w, req, tgt.Local); err != nil {
			if r.serveSpill(w, req, tgt.Local) {
				return
			}
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		}
	case tgt.Peer != "":
		if err := r.proxyPeer(w, req, tgt.Peer, tgt.PeerID); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		}
	default:
		http.Error(w, "no router for requested model", http.StatusNotFound)
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
			serveErr = r.proxyLocal(w, req, tgt.Local)
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

// serveSpill tries later pool targets after a direct local id could not start
// (krea holding the V100, chat named agents-a1 instead of daily-driver).
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
	return false
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
	if _, err := r.Load(modelID); err != nil {
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
	r.proxyTo(w, req, m.stanza.Proxy)
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
		if err := r.peerLoad(p, peerModel); err != nil {
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
	r.proxyTo(w, req, p.BaseURL)
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
func (r *Router) peerLoad(p *config.Peer, modelID string) error {
	body, _ := json.Marshal(map[string]string{"model_id": modelID})
	url := p.BaseURL + "/_router/load"
	resp, err := r.httpc.Post(url, "application/json", bytes.NewReader(body))
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
func (r *Router) proxyTo(w http.ResponseWriter, req *http.Request, baseURL string) {
	u, err := url.Parse(baseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
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
	w.Header().Set("X-Accel-Buffering", "no")
	proxy.ServeHTTP(w, req)
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
	r.mu.Lock()
	type running struct {
		id string
		m  *Managed
	}
	var live []running
	for id, m := range r.managed {
		live = append(live, running{id: id, m: m})
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
		loaded = append(loaded, LoadedModel{ID: item.id, Freshness: freshness, VramMB: vram, GPU: gpu})
	}
	for _, g := range gpus {
		reclaim := staleByGPU[g.Index]
		g.FreeIfStaleEvictedMB = g.FreeMB + reclaim
		if g.TotalMB > 0 && g.FreeIfStaleEvictedMB > g.TotalMB {
			g.FreeIfStaleEvictedMB = g.TotalMB
		}
	}
	return &Telemetry{
		Node:         r.node,
		GPUs:         gpus,
		LoadedModels: loaded,
		Timestamp:    now.Unix(),
	}
}

// HandleLoad is the control endpoint handler for peer load() calls. Accepts
// stanza ids and aliases (so a peer can load "coding-model" on a node whose
// stanza is "qwopus-27b-coder").
func (r *Router) HandleLoad(modelID string) error {
	s := r.cfg.Stanza(modelID)
	if s == nil {
		s = r.cfg.Alias(modelID)
	}
	if s == nil {
		return fmt.Errorf("unknown local model %q", modelID)
	}
	_, err := r.Load(s.ModelID)
	return err
}
