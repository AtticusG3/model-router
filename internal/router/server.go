package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// modelRoutedPaths are the endpoints whose model is resolved from the body /
// query / path by the matcher. Everything else is either a control endpoint or
// 404. Mirrors llama-swap's route table.
var modelRoutedPaths = []string{
	"/v1/chat/completions",
	"/v1/completions",
	"/v1/responses",
	"/v1/messages",
	"/v1/messages/count_tokens",
	"/v1/embeddings",
	"/v1/rerank",
	"/v1/reranking",
	"/rerank",
	"/reranking",
	"/infill",
	"/completion",
	"/v1/audio/speech",
	"/v1/audio/voices",
	"/v1/images/generations",
	"/v1/images/edits",
	"/sdapi/v1/txt2img",
	"/sdapi/v1/img2img",
	"/sdapi/v1/loras",
	"/v/chat/completions",
	"/v/embeddings",
	"/v/responses",
	"/v/completions",
	"/v/messages",
	"/v/rerank",
	"/v/reranking",
	// llama.cpp native WebUI/control endpoints. The WebUI supplies ?model=;
	// requests without a model still fail closed in the catch-all below.
	"/props",
	"/slots",
	"/tokenize",
	"/detokenize",
	"/apply-template",
}

// NewHandler builds the HTTP mux for the router.
func NewHandler(r *Router, logger *Logger) http.Handler {
	mux := http.NewServeMux()

	// Control endpoints.
	mux.HandleFunc("/_router/load", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ModelID string `json:"model_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.ModelID == "" {
			http.Error(w, "model_id required", http.StatusBadRequest)
			return
		}
		if err := r.HandleLoad(body.ModelID); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"loaded"}`)
	})

	mux.HandleFunc("/_router/unload", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ModelID string `json:"model_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.ModelID == "" {
			http.Error(w, "model_id required", http.StatusBadRequest)
			return
		}
		if err := r.HandleUnload(body.ModelID); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		fmt.Fprintln(w, `{"status":"unloaded"}`)
	})

	mux.HandleFunc("/_router/status", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, map[string]any{
			"node":      r.node,
			"models":    r.AllModelStatuses(),
			"telemetry": r.TelemetrySnapshot(),
			"peers":     r.Peers().Snapshot(),
		})
	})

	mux.HandleFunc("/_router/logs", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, map[string]any{"entries": logger.RecentLogs(300)})
	})

	mux.HandleFunc("/_router/telemetry", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, r.TelemetrySnapshot())
	})

	// Embedded operator WebUI. It shares the router listener and needs no
	// separate runtime or static-file service.
	mux.HandleFunc("/ui", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/ui/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/ui/", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, webUIHTML)
	})

	// Health + metrics.
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintln(w, "OK")
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		var sb strings.Builder
		now := time.Now().Unix()
		for _, ms := range r.LocalModelStatuses() {
			switch ms.Type {
			case "model":
				fmt.Fprintf(&sb, "model_router_model_state{model=%q} 1\n", ms.ID)
				fmt.Fprintf(&sb, "model_router_model_vram_mb{model=%q} %d\n", ms.ID, ms.VramMB)
			}
		}
		for _, g := range r.ledger.GPUs() {
			fmt.Fprintf(&sb, "model_router_gpu_free_mb{gpu=%d} %d\n", g.Index, g.FreeMB)
			fmt.Fprintf(&sb, "model_router_gpu_total_mb{gpu=%d} %d\n", g.Index, g.TotalMB)
		}
		fmt.Fprintf(&sb, "model_router_up{node=%q} 1\n", r.node)
		_ = now
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, sb.String())
	})

	// OpenAI /v1/models listing (llama-swap-compatible shape).
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, req *http.Request) {
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
			meta := map[string]any{"modelrouter": map[string]any{"type": ms.Type}}
			rec["meta"] = meta
			rec["status"] = map[string]any{"value": ms.State}
			data = append(data, rec)
		}
		writeJSON(w, map[string]any{"object": "list", "data": data})
	})

	// Model-routed endpoints.
	for _, p := range modelRoutedPaths {
		pp := p
		mux.HandleFunc(pp, func(w http.ResponseWriter, req *http.Request) {
			// Versionless /v/... routes are forwarded as-is; the backend
			// handles them (llama-server supports both).
			r.ServeHTTP(w, req)
		})
	}

	// sdapi passthrough (any /sdapi/v1/* path the matcher can resolve).
	mux.HandleFunc("/sdapi/v1/", func(w http.ResponseWriter, req *http.Request) {
		r.ServeHTTP(w, req)
	})

	// The native llama.cpp WebUI may request root/static paths with a model
	// query parameter. Route those through the same matcher. A browser visiting
	// the bare listener root gets the operator UI instead.
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("model") != "" {
			r.ServeHTTP(w, req)
			return
		}
		if req.Method == http.MethodGet || req.Method == http.MethodHead {
			http.Redirect(w, req, "/ui/", http.StatusFound)
			return
		}
		http.NotFound(w, req)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
