package router

import (
	"encoding/json"
	"fmt"
	"net/http"
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
			writeJSON(w, map[string]any{"entries": logger.RecentLogs(300)})
		}},
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
		if err := r.HandleLoad(id); err != nil {
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
		snap := r.TelemetrySnapshot()
		for _, ms := range r.LocalModelStatuses() {
			if ms.Origin != "local" {
				continue
			}
			fmt.Fprintf(&sb, "model_router_model_state{model=%q} 1\n", ms.ID)
			fmt.Fprintf(&sb, "model_router_model_vram_mb{model=%q} %d\n", ms.ID, ms.VramMB)
		}
		for _, g := range snap.GPUs {
			fmt.Fprintf(&sb, "model_router_gpu_free_mb{gpu=%d} %d\n", g.Index, g.FreeMB)
			fmt.Fprintf(&sb, "model_router_gpu_free_if_stale_evicted_mb{gpu=%d} %d\n", g.Index, g.FreeIfStaleEvictedMB)
			fmt.Fprintf(&sb, "model_router_gpu_total_mb{gpu=%d} %d\n", g.Index, g.TotalMB)
		}
		fmt.Fprintf(&sb, "model_router_up{node=%q} 1\n", r.node)
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
