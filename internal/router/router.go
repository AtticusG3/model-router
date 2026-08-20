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

// evictToFit unloads other models on the stanza's pinned GPU until the ledger
// can admit it. Matches llama-swap matrix exclusivity (krea vs agents-a1 on
// the V100) without a full eviction-cost policy.
func (r *Router) evictToFit(s *config.Stanza) {
	want := deviceIndex(s.Device)
	if want < 0 {
		return
	}
	for _, id := range r.ledger.occupantsOnGPU(want, s.ModelID) {
		if r.busy(id) {
			r.logger.Infof("skip evict %s (in-flight) while admitting %s", id, s.ModelID)
			continue
		}
		r.logger.Infof("evicting %s to admit %s on gpu %d", id, s.ModelID, want)
		if err := r.Unload(id); err != nil {
			r.logger.Errorf("evict %s: %v", id, err)
		}
		if r.ledger.CanAdmit(s) {
			return
		}
	}
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
		if !r.peers.Fresh(p.Name) {
			continue
		}
		vram := r.modelVram(modelID)
		if vram > 0 && !r.peerFits(p.Name, vram) {
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

func (r *Router) modelVram(modelID string) int64 {
	if s := r.cfg.Stanza(modelID); s != nil {
		return s.VramMB
	}
	if s := r.cfg.Alias(modelID); s != nil {
		return s.VramMB
	}
	return 0
}

// peerFits reports whether cached peer telemetry shows a GPU with at least
// vramMB free. Used only to pick a candidate; the peer admits for real.
// Unknown or zero vram (no local stanza/alias) fails closed: FreeMB >= 0
// must not count as a fit.
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
		if g.FreeMB >= vramMB {
			return true
		}
	}
	return false
}

// resolvePool implements spillover selection. Targets are tried in order; a
// target is used when occupancy is below the spillover cap and it is loaded
// or can be admitted locally / reached on a peer. Occupancy increments when
// a target is chosen.
func (r *Router) resolvePool(poolName string) (Target, error) {
	pool := r.cfg.Pool(poolName)
	if pool == nil {
		return Target{}, fmt.Errorf("unknown pool %q", poolName)
	}
	limit := pool.Spillover
	if limit <= 0 {
		limit = 1
	}

	for _, target := range pool.Targets {
		ref := resolveRef(target, r.cfg)
		switch {
		case ref.Local != "":
			t := Target{Local: ref.Local}
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
			if s := r.cfg.Stanza(ref.Local); s != nil && r.ledger.CanAdmit(s) {
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
			if r.peerFits(ref.Peer, r.modelVram(ref.PeerID)) {
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
	return Target{}, fmt.Errorf("no capacity for pool %q (all targets busy/unreachable)", poolName)
}

// ServeHTTP is the entry for model-routed endpoints.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ref, err := matchRequest(req, r.cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tgt, err := r.resolveTarget(ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if ref.Pool != "" {
		defer r.releaseOccupancy(tgt)
	}

	switch {
	case tgt.Local != "":
		r.serveLocal(w, req, tgt.Local)
	case tgt.Peer != "":
		r.servePeer(w, req, tgt.Peer, tgt.PeerID)
	default:
		http.Error(w, "no router for requested model", http.StatusNotFound)
	}
}

// serveLocal proxies to a local backend, spawning it if needed.
func (r *Router) serveLocal(w http.ResponseWriter, req *http.Request, modelID string) {
	r.holdOccupancy(modelID)
	defer r.releaseOccupancy(Target{Local: modelID})
	if _, err := r.Load(modelID); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	r.mu.Lock()
	m := r.managed[modelID]
	r.mu.Unlock()
	if m == nil || m.State() != StateRunning {
		http.Error(w, "model failed to start", http.StatusServiceUnavailable)
		return
	}
	m.markUsed()
	defer m.markUsed()
	r.proxyTo(w, req, m.stanza.Proxy)
}

// servePeer proxies to a peer, first asking it to load the model (the peer runs
// its own admission control — no split-brain).
func (r *Router) servePeer(w http.ResponseWriter, req *http.Request, peerName, peerModel string) {
	p := r.cfg.Peer(peerName)
	if p == nil {
		http.Error(w, fmt.Sprintf("unknown peer %q", peerName), http.StatusServiceUnavailable)
		return
	}
	// OpenAI-kind peers (openrouter etc.) have no /_router control API and no
	// load step — the upstream serves the model id directly.
	if p.Kind != "openai" {
		if err := r.peerLoad(p, peerModel); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	// The request may still name the model as "peerName/peerModel"; the peer
	// only knows its local id, so rewrite the model field before proxying
	// (llama-swap's ReplaceRequestModel does the same). The field rewritten is
	// the peer's configured body_field, not a hardcoded "model".
	if err := rewriteBodyModel(req, peerModel, p.BodyField); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	r.proxyTo(w, req, p.BaseURL)
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

// ReapIdle unloads local backends idle past their TTL. Called on an interval.
func (r *Router) ReapIdle() {
	now := time.Now()
	r.mu.Lock()
	ids := make([]string, 0, len(r.managed))
	for id, m := range r.managed {
		if m.IsIdle(now) {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.logger.Infof("unloading idle model %s", id)
		r.Unload(id)
	}
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
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Type    string `json:"type,omitempty"`   // always "model"
	Origin  string `json:"origin,omitempty"` // local | remote
	State   string `json:"state,omitempty"`
	VramMB  int64  `json:"vram_mb,omitempty"`
	APIType string `json:"api_type,omitempty"`
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
		out = append(out, ModelStatus{
			ID: id, Name: s.Name, Type: "model", Origin: "local",
			State: string(st), VramMB: s.VramMB, APIType: s.APIType,
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

// TelemetrySnapshot assembles this node's telemetry for peers.
func (r *Router) TelemetrySnapshot() *Telemetry {
	gpus := r.ledger.GPUs()
	var loaded []string
	r.mu.Lock()
	for id, m := range r.managed {
		if m.State() == StateRunning {
			loaded = append(loaded, id)
		}
	}
	r.mu.Unlock()
	return &Telemetry{
		Node:         r.node,
		GPUs:         gpus,
		LoadedModels: loaded,
		Timestamp:    time.Now().Unix(),
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
