// fake-model is a tiny OpenAI/sd.cpp-compatible backend used to smoke-test the
// model router without loading real models or touching GPUs. It speaks just
// enough of the API surface: /health, /v1/models, /v1/chat/completions
// (streaming + non-streaming), /v1/embeddings, /v1/rerank, and
// /sdapi/v1/txt2img.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	var (
		port  = flag.Int("port", 5900, "listen port")
		model = flag.String("model", "fake-model", "model id this backend reports")
	)
	flag.Parse()

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "OK")
	})

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"object": "list",
			"data": []map[string]any{{
				"id": *model, "object": "model", "created": time.Now().Unix(),
			}},
		})
	})

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		stream, _ := body["stream"].(bool)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher, _ := w.(http.Flusher)
			for i := 0; i < 3; i++ {
				chunk := map[string]any{
					"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": *model,
					"choices": []map[string]any{{"index": i, "delta": map[string]any{"content": fmt.Sprintf("tok%d ", i)}}},
				}
				fmt.Fprintf(w, "data: %s\n\n", mustJSON(chunk))
				flusher.Flush()
				time.Sleep(200 * time.Millisecond)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		writeJSON(w, map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion", "model": *model,
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "hello from " + *model}}},
		})
	})

	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"object": "list", "model": *model,
			"data": []map[string]any{{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}}},
		})
	})

	mux.HandleFunc("/v1/rerank", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"results": []map[string]any{{"index": 0, "relevance_score": 0.99}}})
	})

	mux.HandleFunc("/sdapi/v1/txt2img", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"images": []string{"iVBORw0KGgo="}, "parameters": map[string]any{"model": *model}})
	})

	log.Printf("fake-model %q listening on :%d", *model, *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), mux))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
