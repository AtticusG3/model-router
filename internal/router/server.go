package router

import (
	"encoding/json"
	"fmt"
	"net/http"
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
		{"/_router/status", func(w http.ResponseWriter, req *http.Request) {
			writeJSON(w, map[string]any{
				"node":      r.node,
				"models":    r.AllModelStatuses(),
				"telemetry": r.TelemetrySnapshot(),
				"peers":     r.Peers().Snapshot(),
			})
		}},
		{"/_router/logs", func(w http.ResponseWriter, req *http.Request) {
			writeJSON(w, map[string]any{
				"entries":         logger.RecentLogs(300),
				"upstream":        logger.RecentUpstream(300),
				"mesh":            logger.RecentMesh(300),
				"backend_metrics": r.BackendMetrics(),
			})
		}},
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
		{"/v1/models", handleModels(r)},
		// Prefixes cover OpenAI /v1, versionless /v, and sdapi. Exact natives
		// still need their own entries because they are not under those trees.
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
		if !ok {
			return
		}
		if err := r.HandleLoad(req.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
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

func handleActivity(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"entries": r.Activity().List()})
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

func decodeModelID(w http.ResponseWriter, req *http.Request) (string, bool) {
	var body struct {
		ModelID string `json:"model_id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.ModelID == "" {
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
		for i := range r.cfg.Peers {
			p := &r.cfg.Peers[i]
			if p.Kind != "router" {
				continue
			}
			fresh := 0
			if r.peers.Fresh(p.Name) {
				fresh = 1
			}
			fmt.Fprintf(&sb, "model_router_peer_fresh{peer=%q} %d\n", p.Name, fresh)
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
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
		ref := resolveRef(id, r.cfg)
		if ref.Local == "" {
			http.Error(w, fmt.Sprintf("unknown local model %q", id), http.StatusNotFound)
			return
		}
		if !slash {
			http.Redirect(w, req, "/upstream/"+id+"/", http.StatusFound)
			return
		}
		req.URL.Path = upPath
		if err := r.proxyLocal(w, req, ref.Local); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
