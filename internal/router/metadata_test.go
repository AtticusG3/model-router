package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testMetadata = `{
  "families": {
    "qwen": {"name": "Qwen", "owner": "Example Qwen Owner", "icon_url": "https://icons.example.test/qwen.svg"},
    "zimage": {"name": "Z-Image", "owner": "Example Image Lab", "icon_url": "https://icons.example.test/zimage.svg"}
  },
  "models": [
    {"match": "qwen-*", "params": "8B", "owner": "Example Tune Co", "licence": "Apache-2.0", "context_length": 8192, "modality": "text"},
    {"match": "z-anime-turbo", "family": "zimage", "params": "6B"}
  ]
}`

// metaRouter is a router whose catalog.metadata_url is url, running its
// metadata loop until the test ends.
func metaRouter(t *testing.T, url string) *Router {
	t.Helper()
	r := spilloverRouter(t, `
start_port: 5900
catalog:
  metadata_url: "`+url+`"
  refresh_seconds: 1
stanzas:
  - model_id: qwen-tune-27b
    command: "x --port {port} --model /m/qwen-tune-27b-Q4_K_M.gguf --ctx-size 32768"
    params: 27B
    owner: Example Stanza Owner
  - model_id: z-anime-turbo
    api_type: image
    command: "x --port {port} --steps 8 -W 1024 -H 768"
`)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.RunMetadata(ctx)
	return r
}

func catalogRow(r *Router, id string) ModelStatus {
	for _, ms := range r.AllModelStatuses() {
		if ms.ID == id {
			return ms
		}
	}
	return ModelStatus{}
}

func fastMetadataRetry(t *testing.T) {
	old := metadataRetry
	metadataRetry = 5 * time.Millisecond
	t.Cleanup(func() { metadataRetry = old })
}

// A served metadata file fills in owner, params and icon; stanza params and
// owner win over it, and the command line gives quant, context and image size.
func TestCatalogMetadataFromServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(testMetadata))
	}))
	defer srv.Close()
	r := metaRouter(t, srv.URL)
	waitFor(t, "metadata", func() bool { return r.meta.Load() != nil })

	q := catalogRow(r, "qwen-tune-27b")
	if q.Family == nil || q.Family.Owner != "Example Qwen Owner" {
		t.Errorf("qwen family: %+v", q.Family)
	}
	want := ModelInfo{IconURL: "https://icons.example.test/qwen.svg", Owner: "Example Stanza Owner", Params: "27B", Licence: "Apache-2.0", ContextLength: 32768, Modality: "text", Quant: "Q4_K_M"}
	if q.Info == nil || *q.Info != want {
		t.Errorf("qwen info: %+v, want %+v", q.Info, want)
	}

	z := catalogRow(r, "z-anime-turbo")
	if z.Family == nil || z.Family.Name != "Z-Image" {
		t.Errorf("z-anime family from the file: %+v", z.Family)
	}
	want = ModelInfo{IconURL: "https://icons.example.test/zimage.svg", Owner: "Example Image Lab", Params: "6B", Size: "1024x768", Steps: 8}
	if z.Info == nil || *z.Info != want {
		t.Errorf("z-anime info: %+v, want %+v", z.Info, want)
	}
}

// An unreachable file leaves built-in data in the catalog, logs one warning
// however many tries fail, and is fetched once it answers again.
func TestCatalogMetadataUnreachableRetries(t *testing.T) {
	fastMetadataRetry(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 3 {
			panic(http.ErrAbortHandler) // drop the connection, as an unreachable host would
		}
		w.Write([]byte(testMetadata))
	}))
	defer srv.Close()
	r := metaRouter(t, srv.URL)
	waitFor(t, "a failed fetch", func() bool { return hits.Load() >= 1 })
	if q := catalogRow(r, "qwen-tune-27b"); q.Family == nil || q.Family.Owner != "Alibaba" || q.Info.Params != "27B" {
		t.Errorf("built-in data while unreachable: %+v %+v", q.Family, q.Info)
	}

	waitFor(t, "the retried fetch", func() bool { return r.meta.Load() != nil })
	if q := catalogRow(r, "qwen-tune-27b"); q.Family.Owner != "Example Qwen Owner" {
		t.Errorf("family after the retry: %+v", q.Family)
	}
	warnings := 0
	for _, l := range r.logger.RecentLogs(100) {
		if strings.Contains(l, "metadata fetch failed") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("%d warnings for 3 failed tries, want 1", warnings)
	}
}

// The shipped example parses and every match is a valid glob.
func TestCatalogMetadataExampleParses(t *testing.T) {
	raw, err := os.ReadFile("../../configs/model-metadata.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var md metadataFile
	if err := json.Unmarshal(raw, &md); err != nil || len(md.Models) == 0 {
		t.Fatalf("example: %v, %d models", err, len(md.Models))
	}
	for _, m := range md.Models {
		if _, err := path.Match(m.Match, ""); err != nil {
			t.Errorf("match %q: %v", m.Match, err)
		}
	}
}

// An oversized or invalid body is rejected and the last good copy stays.
func TestCatalogMetadataBadBodyKeepsLastGood(t *testing.T) {
	fastMetadataRetry(t)
	bodies := []string{testMetadata, `{"models": [` + strings.Repeat(" ", metadataMaxBytes) + `]}`, `{"models": [`}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(bodies[min(int(hits.Add(1))-1, len(bodies)-1)]))
	}))
	defer srv.Close()
	r := metaRouter(t, srv.URL)
	waitFor(t, "the good copy", func() bool { return r.meta.Load() != nil })
	good := r.meta.Load()
	waitFor(t, "both bad bodies", func() bool { return hits.Load() >= 3 })
	if r.meta.Load() != good {
		t.Fatal("a bad body replaced the last good copy")
	}
	if q := catalogRow(r, "qwen-tune-27b"); q.Info.IconURL == "" {
		t.Errorf("icon lost after bad bodies: %+v", q.Info)
	}
	for _, b := range bodies[1:] {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(b)) }))
		_, err := fetchMetadata(context.Background(), bad.URL)
		bad.Close()
		if err == nil {
			t.Errorf("body of %d bytes accepted", len(b))
		}
	}
}
