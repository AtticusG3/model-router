package router

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// NewHandler builds the HTTP mux for the router.
func NewHandler(r *Router, logger *Logger) http.Handler {
	mux := http.NewServeMux()
	proxy := http.HandlerFunc(r.ServeHTTP)

	routes := []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"/_router/load", handleLoad(r)},
		{"/_router/unload", handleUnload(r)},
		{"/_router/reload", handleReload(r)},
		{"/_router/status", func(w http.ResponseWriter, req *http.Request) {
			peers := r.Peers().Snapshot()
			inPlay := make(map[string]bool, len(peers))
			for _, p := range r.PeerList() {
				inPlay[p.Name] = true
			}
			for name := range peers {
				if !inPlay[name] {
					delete(peers, name)
				}
			}
			// Freshness is judged on this node's clock, not the peer's timestamp.
			fresh := make(map[string]bool, len(peers))
			for name := range peers {
				fresh[name] = r.Peers().Fresh(name)
			}
			writeJSON(w, map[string]any{
				"node":        r.node,
				"models":      r.AllModelStatuses(),
				"telemetry":   r.TelemetrySnapshot(),
				"peers":       peers,
				"peers_fresh": fresh,
				"discovered":  r.DiscoveredPeers(),
				"reload":      r.reloadStatus(),
				"jobs":        r.RunningJobs(),
			})
		}},
		{"/_router/backend-metrics", func(w http.ResponseWriter, req *http.Request) {
			writeJSON(w, map[string]any{"backends": r.BackendSummaries()})
		}},
		{"/_router/logs", func(w http.ResponseWriter, req *http.Request) {
			writeJSON(w, map[string]any{
				"entries":         logger.RecentLogs(300),
				"upstream":        logger.RecentUpstream(300),
				"mesh":            logger.RecentMesh(300),
				"backend_metrics": r.BackendMetrics(),
			})
		}},
		{"/_router/logs/{model_id...}", handleModelLogSnapshot(r, logger)},
		{"/logs", handleLogs(logger)},
		{"/logs/stream", handleLogStream(logger, "", "")},
		{"/logs/stream/proxy", handleLogStream(logger, streamProxy, "")},
		{"/logs/stream/upstream", handleLogStream(logger, streamUpstream, "")},
		{"/logs/stream/mesh", handleLogStream(logger, streamMesh, "")},
		{"/logs/stream/{model_id...}", handleModelLogStream(r, logger)},
		{"/_router/activity/{id}/thumb", handleActivityBlob(r)},
		{"/_router/activity/{id}/audio", handleActivityBlob(r)},
		{"/_router/activity/{id}/image/{side}/{n}", handleActivityBlob(r)},
		{"/_router/activity/stream", handleActivityStream(r)},
		{"/_router/activity/{id}", handleActivityCapture(r)},
		{"/_router/activity", handleActivity(r)},
		{"/_router/telemetry", func(w http.ResponseWriter, req *http.Request) {
			writeJSON(w, r.TelemetrySnapshot())
		}},
		{"/ui", func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "/ui/", http.StatusMovedPermanently)
		}},
		{"/ui/", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, webUIHTML)
		}},
		{"/upstream", func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "/ui/", http.StatusFound)
		}},
		{"/upstream/{path...}", handleUpstream(r)},
		{"/health", func(w http.ResponseWriter, req *http.Request) {
			fmt.Fprintln(w, "OK")
		}},
		{"/metrics", handleMetrics(r)},
		// Alias for the UI link: a fronting proxy may own /metrics but passes /_router/* through.
		{"/_router/metrics", handleMetrics(r)},
		{"/v1/models", handleModels(r)},
		// Prefixes cover OpenAI /v1, versionless /v, and sdapi. Exact natives
		// still need their own entries because they are not under those trees.
		// /v1/realtime upgrades to a websocket through this proxy (see
		// sessionWriter and answerUpgrade in realtime.go).
		{"/v1/{path...}", proxy},
		{"/v/{path...}", proxy},
		{"/sdapi/v1/", proxy},
		{"/rerank", proxy},
		{"/reranking", proxy},
		{"/infill", proxy},
		{"/completion", proxy},
		{"/props", proxy},
		{"/slots", proxy},
		{"/tokenize", proxy},
		{"/detokenize", proxy},
		{"/apply-template", proxy},
		{"/", handleRoot(r)},
	}
	for _, rt := range routes {
		mux.HandleFunc(rt.pattern, rt.h)
	}
	return mux
}

func handleLoad(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id, ok := decodeModelID(w, req)
		if !ok || r.refuseUnshared(w, req, id) {
			return
		}
		if err := r.HandleLoad(req.Context(), id); err != nil {
			loadFailed(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"loaded"}`)
	}
}

func handleUnload(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id, ok := decodeModelID(w, req)
		if !ok {
			return
		}
		if err := r.Unload(id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		fmt.Fprintln(w, `{"status":"unloaded"}`)
	}
}

// reloadStatus is the last reload's outcome for the UI; nil until one has run.
func (r *Router) reloadStatus() map[string]any {
	if r.reloader == nil {
		return nil
	}
	at, errMsg := r.reloader.LastReload()
	if at.IsZero() {
		return nil
	}
	return map[string]any{"at": at.Format(time.RFC3339), "ok": errMsg == "", "error": errMsg}
}

// handleReload runs the same validate-then-apply path as the file watch and
// SIGHUP, from the current config path. The body is ignored. A rejected file
// is 422 and leaves the running config as it was.
func handleReload(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.reloader == nil {
			http.Error(w, "config reload is not enabled", http.StatusServiceUnavailable)
			return
		}
		err := r.reloader.Reload()
		out := r.reloadStatus() // set by Reload, so never nil here
		out["status"] = "reloaded"
		if err != nil {
			out["status"] = "rejected"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
		writeJSON(w, out)
	}
}

func handleActivity(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Running before finished: a request ending in between then shows in
		// both lists under one id, never in neither.
		a := r.Activity()
		running := a.InProgress()
		writeJSON(w, map[string]any{"entries": a.List(), "in_progress": running})
	}
}

// handleActivityStream is the Activity page's live feed (SSE): a snapshot
// event shaped like GET /_router/activity, then one entry event per recorded
// row. Event names leave room for in-progress updates on the same stream.
func handleActivityStream(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		a := r.Activity()
		// Subscribe first: a request that finishes after it arrives as an entry
		// event, so nothing in the snapshot's in_progress list is left running.
		rows, live, cancel := a.Subscribe()
		defer cancel()
		running := a.InProgress()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		send := func(event string, v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			fl.Flush()
		}
		send("snapshot", map[string]any{"entries": rows, "in_progress": running})
		for {
			select {
			case <-req.Context().Done():
				return
			case e, ok := <-live:
				if !ok {
					return
				}
				send("entry", e)
			}
		}
	}
}

func handleActivityCapture(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id, err := strconv.Atoi(req.PathValue("id"))
		if err != nil || id < 1 {
			http.Error(w, "invalid activity id", http.StatusBadRequest)
			return
		}
		cap, ok := r.Activity().Capture(id)
		if !ok {
			http.Error(w, "capture not found", http.StatusNotFound)
			return
		}
		writeJSON(w, cap)
	}
}

// handleActivityBlob serves the row's image thumbnail or speech audio, the one
// blob a capture keeps, or with {side} and {n} one stored image preview.
func handleActivityBlob(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id, err := strconv.Atoi(req.PathValue("id"))
		if err != nil || id < 1 {
			http.Error(w, "invalid activity id", http.StatusBadRequest)
			return
		}
		var data []byte
		var ct string
		var ok bool
		if side := req.PathValue("side"); side != "" {
			s := slices.Index(previewSides, side)
			n, err := strconv.Atoi(req.PathValue("n"))
			if s < 0 || err != nil {
				http.Error(w, "invalid image", http.StatusBadRequest)
				return
			}
			data, ct, ok = r.Activity().Preview(id, s, n)
		} else {
			data, ct, ok = r.Activity().Blob(id)
		}
		if !ok {
			http.Error(w, "not captured", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", ct)
		// The capture lives until the ring evicts it, so a short cache is safe.
		w.Header().Set("Cache-Control", "private, max-age=600")
		_, _ = w.Write(data)
	}
}

// maxControlBodyBytes caps the JSON body of /_router/load and /_router/unload.
const maxControlBodyBytes = 1 << 20

func decodeModelID(w http.ResponseWriter, req *http.Request) (string, bool) {
	var body struct {
		ModelID string `json:"model_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxControlBodyBytes)).Decode(&body); err != nil || body.ModelID == "" {
		http.Error(w, "model_id required", http.StatusBadRequest)
		return "", false
	}
	return body.ModelID, true
}

func handleMetrics(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var sb strings.Builder
		fmt.Fprintf(&sb, "# HELP model_router_up 1 if this process is serving\n")
		fmt.Fprintf(&sb, "# TYPE model_router_up gauge\n")
		fmt.Fprintf(&sb, "model_router_up{node=%q} 1\n", r.node)

		fmt.Fprintf(&sb, "# HELP model_router_model_running 1 if the local backend is running\n")
		fmt.Fprintf(&sb, "# TYPE model_router_model_running gauge\n")
		fmt.Fprintf(&sb, "# HELP model_router_model_vram_mb stanza reservation in MB\n")
		fmt.Fprintf(&sb, "# TYPE model_router_model_vram_mb gauge\n")
		for _, ms := range r.LocalModelStatuses() {
			if ms.Origin != "local" {
				continue
			}
			running := 0
			if ms.State == string(StateRunning) {
				running = 1
			}
			fmt.Fprintf(&sb, "model_router_model_running{model=%q} %d\n", ms.ID, running)
			fmt.Fprintf(&sb, "model_router_model_vram_mb{model=%q} %d\n", ms.ID, ms.VramMB)
		}

		fmt.Fprintf(&sb, "# HELP model_router_jobs_running async jobs holding a model loaded\n")
		fmt.Fprintf(&sb, "# TYPE model_router_jobs_running gauge\n")
		jobs := r.RunningJobs()
		for _, id := range slices.Sorted(maps.Keys(jobs)) {
			fmt.Fprintf(&sb, "model_router_jobs_running{model=%q} %d\n", id, jobs[id])
		}

		snap := r.TelemetrySnapshot()
		fmt.Fprintf(&sb, "# HELP model_router_gpu_free_mb live nvidia-smi free VRAM\n")
		fmt.Fprintf(&sb, "# TYPE model_router_gpu_free_mb gauge\n")
		fmt.Fprintf(&sb, "# HELP model_router_gpu_total_mb GPU memory total\n")
		fmt.Fprintf(&sb, "# TYPE model_router_gpu_total_mb gauge\n")
		fmt.Fprintf(&sb, "# HELP model_router_gpu_free_if_stale_evicted_mb free plus stale-model reservations\n")
		fmt.Fprintf(&sb, "# TYPE model_router_gpu_free_if_stale_evicted_mb gauge\n")
		fmt.Fprintf(&sb, "# HELP model_router_gpu_free_if_idle_evicted_mb free plus all loaded-model reservations\n")
		fmt.Fprintf(&sb, "# TYPE model_router_gpu_free_if_idle_evicted_mb gauge\n")
		for _, g := range snap.GPUs {
			fmt.Fprintf(&sb, "model_router_gpu_free_mb{gpu=%d} %d\n", g.Index, g.FreeMB)
			fmt.Fprintf(&sb, "model_router_gpu_total_mb{gpu=%d} %d\n", g.Index, g.TotalMB)
			fmt.Fprintf(&sb, "model_router_gpu_free_if_stale_evicted_mb{gpu=%d} %d\n", g.Index, g.FreeIfStaleEvictedMB)
			fmt.Fprintf(&sb, "model_router_gpu_free_if_idle_evicted_mb{gpu=%d} %d\n", g.Index, g.FreeIfIdleEvictedMB)
		}

		fmt.Fprintf(&sb, "# HELP model_router_peer_fresh 1 if peer telemetry is within the stale window\n")
		fmt.Fprintf(&sb, "# TYPE model_router_peer_fresh gauge\n")
		for _, p := range r.PeerList() {
			if p.Kind != "router" {
				continue
			}
			fresh := 0
			if r.peers.Fresh(p.Name) {
				fresh = 1
			}
			fmt.Fprintf(&sb, "model_router_peer_fresh{peer=%q} %d\n", p.Name, fresh)
		}

		// Request counters, latency histograms and token totals. Kept in the
		// same hand-rolled exposition as the gauges above so the binary stays
		// dependency-free.
		if r.metrics != nil {
			sb.WriteString(r.metrics.Render(r.node))
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, sb.String())
	}
}

func handleModels(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		created := time.Now().Unix()
		var data []map[string]any
		for _, ms := range r.LocalModelStatuses() {
			rec := map[string]any{
				"id":       ms.ID,
				"object":   "model",
				"created":  created,
				"owned_by": "model-router",
			}
			if ms.Name != "" {
				rec["name"] = ms.Name
			}
			if len(ms.Capabilities) > 0 {
				rec["capabilities"] = ms.Capabilities
			}
			meta := map[string]any{"modelrouter": map[string]any{"type": "model", "origin": ms.Origin}}
			rec["meta"] = meta
			rec["status"] = map[string]any{"value": ms.State}
			data = append(data, rec)
		}
		writeJSON(w, map[string]any{"object": "list", "data": data})
	}
}

// handleUpstream reverse-proxies to a local backend after stripping
// /upstream/{model_id}, same as llama-swap's /upstream/:model_id/*.
func handleUpstream(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id, upPath, slash := splitUpstreamPath(req.URL.Path)
		if id == "" {
			http.Redirect(w, req, "/ui/", http.StatusFound)
			return
		}
		ref := resolveRef(id, r.cfg())
		if ref.Local == "" {
			http.Error(w, fmt.Sprintf("unknown local model %q", id), http.StatusNotFound)
			return
		}
		if r.refuseUnshared(w, req, ref.Local) {
			return
		}
		if !slash {
			http.Redirect(w, req, "/upstream/"+id+"/", http.StatusFound)
			return
		}
		req.URL.Path = upPath
		if err := r.proxyLocal(w, req, ref.Local); err != nil {
			loadFailed(w, err)
		}
	}
}

func splitUpstreamPath(path string) (id, upPath string, hadSlash bool) {
	rest := strings.TrimPrefix(path, "/upstream")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return "", "", false
	}
	id, after, hadSlash := strings.Cut(rest, "/")
	if !hadSlash {
		return id, "", false
	}
	if after == "" {
		return id, "/", true
	}
	return id, "/" + after, true
}

func handleRoot(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("model") != "" {
			r.ServeHTTP(w, req)
			return
		}
		if req.Method == http.MethodGet || req.Method == http.MethodHead {
			http.Redirect(w, req, "/ui/", http.StatusFound)
			return
		}
		http.NotFound(w, req)
	}
}

// handleLogs serves the buffered combined log as plain text; browsers are sent
// to the UI instead.
func handleLogs(logger *Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.Header.Get("Accept"), "text/html") {
			http.Redirect(w, req, "/ui/", http.StatusFound)
			return
		}
		history, _, cancel := logger.Subscribe("", "")
		cancel()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, line := range history {
			fmt.Fprintln(w, line)
		}
	}
}

// handleModelLogStream streams one model's backend output. Ids may contain
// slashes. A model literally named proxy, upstream or mesh is shadowed by the
// filter paths.
func handleModelLogStream(r *Router, logger *Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id := req.PathValue("model_id")
		if r.cfg().Stanza(id) == nil {
			http.Error(w, "unknown model id: "+id, http.StatusNotFound)
			return
		}
		handleLogStream(logger, "", id)(w, req)
	}
}

// handleModelLogSnapshot serves one model's log history as JSON for the UI's
// polling fallback: a proxy that buffers chunked text/plain never answers
// /logs/stream/{model_id}, and the combined snapshot has no per-model lines.
// Ids may contain slashes, same as the stream path.
func handleModelLogSnapshot(r *Router, logger *Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id := req.PathValue("model_id")
		if r.cfg().Stanza(id) == nil {
			http.Error(w, "unknown model id: "+id, http.StatusNotFound)
			return
		}
		lines, _, cancel := logger.Subscribe("", id)
		cancel()
		writeJSON(w, map[string]any{"model": id, "lines": lines})
	}
}

// handleLogStream holds the connection open, sends history first (unless
// ?no-history is present), then flushes each new line as it is logged.
func handleLogStream(logger *Logger, stream, model string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		history, live, cancel := logger.Subscribe(stream, model)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		if _, skip := req.URL.Query()["no-history"]; !skip {
			for _, line := range history {
				fmt.Fprintln(w, line)
			}
		}
		fl.Flush()
		for {
			select {
			case <-req.Context().Done():
				return
			case line := <-live:
				fmt.Fprintln(w, line)
				fl.Flush()
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
