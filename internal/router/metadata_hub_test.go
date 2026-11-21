package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

const testHubToken = "hf_placeholder_not_a_real_token"

// Fixture bodies for the fake Hugging Face and ModelScope servers.
const hfMysteryBody = `{"author": "Example-Org", "pipeline_tag": "text-generation",
  "cardData": {"license": "apache-2.0"}, "safetensors": {"total": 27000000000}}`

const hfGGUFBody = `{"author": "Qwen", "pipeline_tag": "text-generation",
  "cardData": {"license": ["apache-2.0"], "base_model": "Qwen/Qwen3-4B"},
  "gguf": {"total": 4022468096, "context_length": 40960}}`

const msMysteryBody = `{"Code": 200, "Data": {"Path": "Example-Org", "License": "Apache License 2.0",
  "Tasks": [{"Name": "text-generation"}], "ModelInfos": {"safetensor": {"total": 27000000000}}}}`

// fakeHub answers fixed bodies by path (pathBody is path, body, path, body...;
// 404 otherwise) and records each request's path and Authorization header.
type fakeHub struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
	auth  []string
}

func newFakeHub(t *testing.T, pathBody ...string) *fakeHub {
	t.Helper()
	bodies := map[string]string{}
	for i := 0; i+1 < len(pathBody); i += 2 {
		bodies[pathBody[i]] = pathBody[i+1]
	}
	h := &fakeHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, req.URL.Path)
		h.auth = append(h.auth, req.Header.Get("Authorization"))
		h.mu.Unlock()
		b, ok := bodies[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write([]byte(b))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *fakeHub) requests() (paths, auth []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.paths), slices.Clone(h.auth)
}

func fastHub(t *testing.T) {
	oldDelay, oldRetry := hubDelay, hubRetry
	hubDelay, hubRetry = 0, 5*time.Millisecond
	t.Cleanup(func() { hubDelay, hubRetry = oldDelay, oldRetry })
}

// hubRouter is a router with the given catalog block and stanzas, running its
// metadata and hub loops until the test ends.
func hubRouter(t *testing.T, catalog, stanzas string) *Router {
	t.Helper()
	r := spilloverRouter(t, "start_port: 5900\ncatalog:\n"+catalog+"\nstanzas:\n"+stanzas)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.RunMetadata(ctx)
	go r.RunHub(ctx)
	return r
}

const mysteryStanza = `
  - model_id: mystery-27b
    command: "x --port {port}"
    hf_repo: Example-Org/Mystery-27B
`

// rowParams is a catalog row's params, empty when the row or its info is missing.
func rowParams(r *Router, id string) string {
	if i := catalogRow(r, id).Info; i != nil {
		return i.Params
	}
	return ""
}

func hubWarnings(r *Router) int {
	n := 0
	for _, l := range r.logger.RecentLogs(200) {
		if strings.Contains(l, "lookup of") && strings.Contains(l, "failed") {
			n++
		}
	}
	return n
}

// Hugging Face fills owner, modality, licence, params and the avatar; a GGUF
// repo fills params and context; a stanza, the metadata file and the command
// line still win; a peer's advertised hf_repo fills its row; the token goes to
// Hugging Face and is never shown.
func TestHubHuggingFaceFillsCatalog(t *testing.T) {
	fastHub(t)
	t.Setenv("HF_TOKEN", testHubToken)
	hf := newFakeHub(t,
		"/api/models/Example-Org/Mystery-27B", hfMysteryBody,
		"/api/organizations/Example-Org/avatar", `{"avatarUrl": "https://avatars.example.test/example-org.png"}`,
		"/api/models/Qwen/Qwen3-4B-GGUF", hfGGUFBody,
		"/api/users/Qwen/avatar", `{"avatarUrl": "/avatars/qwen.png"}`,
		"/api/models/Example-Org/Remote-Tune-12B", hfMysteryBody,
	)
	file := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"models": [{"match": "file-wins", "params": "9B", "licence": "Example-Licence-1.0"}]}`))
	}))
	defer file.Close()
	r := hubRouter(t, `
  metadata_url: "`+file.URL+`"
  hub: huggingface
  hf_base: "`+hf.URL+`"
  hub_token_env: HF_TOKEN
peers:
  - name: alpha
    kind: router
    base_url: http://127.0.0.1:9
    models: [remote-tune]`, mysteryStanza+`
  - model_id: qwen3-4b
    command: "x --port {port} -m /models/hub/models--Qwen--Qwen3-4B-GGUF/snapshots/abc123/Qwen3-4B-Q4_K_M.gguf"
  - model_id: qwen3-4b-bare
    command: "x --port {port} -m /models/Qwen3-4B-Q4_K_M.gguf --ctx-size 8192"
  - model_id: stanza-wins
    command: "x --port {port}"
    hf_repo: Example-Org/Mystery-27B
    params: 30B
    owner: Example Stanza Owner
  - model_id: file-wins
    command: "x --port {port}"
    hf_repo: Example-Org/Mystery-27B
`)
	r.peers.Set("alpha", &Telemetry{Node: "alpha", HFRepos: map[string]string{"remote-tune": "Example-Org/Remote-Tune-12B"}})
	waitFor(t, "hub data", func() bool { return rowParams(r, "qwen3-4b") != "" && rowParams(r, "remote-tune") != "" })
	waitFor(t, "the metadata file", func() bool { return r.meta.Load() != nil })

	avatar, hfLink := "https://avatars.example.test/example-org.png", "https://huggingface.co/Example-Org/Mystery-27B"
	for _, tc := range []struct {
		id   string
		want ModelInfo
	}{
		{"mystery-27b", ModelInfo{IconURL: avatar, Owner: "Example-Org", Params: "27B",
			Licence: "apache-2.0", Modality: "text-generation", Hub: "Hugging Face", HubURL: hfLink}},
		// A built-in family keeps its bundled icon; the command line's quant stays.
		{"qwen3-4b", ModelInfo{Owner: "Qwen", Params: "4B", Licence: "apache-2.0", ContextLength: 40960,
			Modality: "text-generation", Quant: "Q4_K_M", Hub: "Hugging Face", HubURL: "https://huggingface.co/Qwen/Qwen3-4B-GGUF"}},
		{"qwen3-4b-bare", ModelInfo{Owner: "Alibaba", ContextLength: 8192, Quant: "Q4_K_M"}},
		{"stanza-wins", ModelInfo{IconURL: avatar, Owner: "Example Stanza Owner", Params: "30B",
			Licence: "apache-2.0", Modality: "text-generation", Hub: "Hugging Face", HubURL: hfLink}},
		{"file-wins", ModelInfo{IconURL: avatar, Owner: "Example-Org", Params: "9B",
			Licence: "Example-Licence-1.0", Modality: "text-generation", Hub: "Hugging Face", HubURL: hfLink}},
		{"remote-tune", ModelInfo{IconURL: avatar, Owner: "Example-Org", Params: "27B", Licence: "apache-2.0",
			Modality: "text-generation", Hub: "Hugging Face", HubURL: "https://huggingface.co/Example-Org/Remote-Tune-12B"}},
	} {
		if got := catalogRow(r, tc.id).Info; got == nil || *got != tc.want {
			t.Errorf("%s info: %+v, want %+v", tc.id, got, tc.want)
		}
	}

	paths, auth := hf.requests()
	for i, p := range paths {
		if auth[i] != "Bearer "+testHubToken {
			t.Errorf("%s sent Authorization %q", p, auth[i])
		}
		if strings.Contains(p, "Q4_K_M") || strings.HasSuffix(p, "/Qwen/Qwen3-4B") {
			t.Errorf("unexpected lookup (a bare GGUF name, or the base of a repo with params): %s", p)
		}
	}
	if got := r.localHFRepos(); got["qwen3-4b"] != "Qwen/Qwen3-4B-GGUF" || got["mystery-27b"] != "Example-Org/Mystery-27B" || got["qwen3-4b-bare"] != "" {
		t.Errorf("advertised hf_repos: %v", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/_router/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	NewHandler(r, r.logger).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), testHubToken) {
		t.Errorf("status %d, leaks the token: %v", rec.Code, strings.Contains(rec.Body.String(), testHubToken))
	}
	for _, l := range r.logger.RecentLogs(200) {
		if strings.Contains(l, testHubToken) {
			t.Errorf("log line leaks the token: %s", l)
		}
	}
}

// A quant repo with no parameter count of its own takes its base model's.
func TestHubBaseModelParams(t *testing.T) {
	fastHub(t)
	hf := newFakeHub(t,
		"/api/models/Example-Org/Tune-GGUF", `{"author": "Example-Org", "cardData": {"base_model": ["Example-Org/Base-7B"]}}`,
		"/api/models/Example-Org/Base-7B", `{"safetensors": {"total": 7615616512}}`,
		"/api/organizations/Example-Org/avatar", `{}`,
	)
	h, err := fetchHF(context.Background(), config.Catalog{HFBase: hf.URL}, "Example-Org/Tune-GGUF")
	if err != nil || h.Params != "7.6B" || h.Owner != "Example-Org" || h.IconURL != "" {
		t.Errorf("tune: %+v, %v", h, err)
	}
}

// Hugging Face answers 404: with modelscope_fallback the same id fills the row
// from ModelScope, without the token; without it, ModelScope is never asked.
func TestHubModelScopeFallback(t *testing.T) {
	fastHub(t)
	t.Setenv("HF_TOKEN", testHubToken)
	for _, fallback := range []bool{true, false} {
		hf := newFakeHub(t)
		ms := newFakeHub(t, "/api/v1/models/Example-Org/Mystery-27B", msMysteryBody)
		r := hubRouter(t, `
  hub: both
  hf_base: "`+hf.URL+`"
  modelscope_base: "`+ms.URL+`"
  hub_token_env: HF_TOKEN
  modelscope_fallback: `+strconv.FormatBool(fallback), mysteryStanza)
		if !fallback {
			waitFor(t, "two Hugging Face tries", func() bool {
				p, _ := hf.requests()
				return len(p) >= 2
			})
			if p, _ := ms.requests(); len(p) != 0 {
				t.Errorf("ModelScope asked without the fallback: %v", p)
			}
			continue
		}
		waitFor(t, "ModelScope data", func() bool { return rowParams(r, "mystery-27b") != "" })
		want := ModelInfo{Owner: "Example-Org", Params: "27B", Licence: "Apache License 2.0", Modality: "text-generation",
			Hub: "ModelScope", HubURL: "https://modelscope.cn/models/Example-Org/Mystery-27B"}
		if got := catalogRow(r, "mystery-27b").Info; *got != want {
			t.Errorf("info: %+v, want %+v", got, want)
		}
		if _, auth := ms.requests(); slices.ContainsFunc(auth, func(a string) bool { return a != "" }) {
			t.Errorf("ModelScope saw an Authorization header: %v", auth)
		}
	}
}

// With the hub unreachable the catalog keeps built-in data, warns once per
// repo and retries; with cache_file, a restart shows the last good data.
func TestHubOfflineUsesCacheFile(t *testing.T) {
	fastHub(t)
	cache := filepath.Join(t.TempDir(), "hub-metadata.json")
	catalog := func(base string) string {
		return "  hub: huggingface\n  hf_base: \"" + base + "\"\n  cache_file: \"" + cache + "\""
	}
	stanzas := mysteryStanza + `
  - model_id: qwen-other
    command: "x --port {port} --model Qwen/Qwen3-0.6B"
`
	live := newFakeHub(t, "/api/models/Example-Org/Mystery-27B", hfMysteryBody)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := spilloverRouter(t, "start_port: 5900\ncatalog:\n"+catalog(live.URL)+"\nstanzas:\n"+stanzas)
	go first.RunHub(ctx)
	var saved map[string]hubEntry
	waitFor(t, "the first run's cache file", func() bool {
		raw, err := os.ReadFile(cache)
		return err == nil && json.Unmarshal(raw, &saved) == nil && saved["huggingface:Example-Org/Mystery-27B"].Info.Params == "27B"
	})
	cancel()

	// Age the saved copy so the restart wants to fetch it again.
	for k, e := range saved {
		e.Fetched = time.Time{}
		saved[k] = e
	}
	if err := writeHubCache(cache, saved); err != nil {
		t.Fatal(err)
	}

	var hits atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
		panic(http.ErrAbortHandler) // drop the connection, as an unreachable host would
	}))
	defer down.Close()
	r := hubRouter(t, catalog(down.URL), stanzas)
	waitFor(t, "retries of both repos", func() bool { return hits.Load() >= 6 })
	if got := catalogRow(r, "mystery-27b").Info; got.Params != "27B" || got.Owner != "Example-Org" {
		t.Errorf("cached data after a restart: %+v", got)
	}
	if got := catalogRow(r, "qwen-other"); got.Family == nil || got.Family.Owner != "Alibaba" || got.Info.Owner != "Alibaba" || got.Info.Params != "" {
		t.Errorf("built-in data while offline: %+v %+v", got.Family, got.Info)
	}
	if n := hubWarnings(r); n != 2 {
		t.Errorf("%d warnings for 2 repos, want 2", n)
	}
}

// An oversized or invalid body is rejected and the last good copy stays.
func TestHubBadBodyKeepsLastGood(t *testing.T) {
	fastHub(t)
	cache := filepath.Join(t.TempDir(), "hub.json")
	if err := writeHubCache(cache, map[string]hubEntry{"huggingface:Example-Org/Mystery-27B": {Info: hubInfo{Params: "27B"}}}); err != nil {
		t.Fatal(err)
	}
	bodies := []string{`{"author": "` + strings.Repeat("x", metadataMaxBytes) + `"}`, `{"author": `}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(bodies[min(int(hits.Add(1))-1, len(bodies)-1)]))
	}))
	defer srv.Close()
	r := hubRouter(t, "  hub: huggingface\n  hf_base: \""+srv.URL+"\"\n  cache_file: \""+cache+"\"", mysteryStanza)
	waitFor(t, "both bad bodies", func() bool { return hits.Load() >= 3 })
	if got := rowParams(r, "mystery-27b"); got != "27B" {
		t.Errorf("a bad body replaced the last good copy: params %q", got)
	}
	c := r.cfg().Catalog
	for _, b := range bodies {
		c.HFBase = newFakeHub(t, "/api/models/Example-Org/Mystery-27B", b).URL
		if _, err := fetchHF(context.Background(), c, "Example-Org/Mystery-27B"); err == nil {
			t.Errorf("body of %d bytes accepted", len(b))
		}
	}
}

// hub: off (the default) makes no hub request, but a known repo is still linked.
func TestHubOffMakesNoRequests(t *testing.T) {
	fastHub(t)
	hf := newFakeHub(t, "/api/models/Example-Org/Mystery-27B", hfMysteryBody)
	r := hubRouter(t, "  hf_base: \""+hf.URL+"\"\n  modelscope_fallback: true", mysteryStanza)
	time.Sleep(100 * time.Millisecond)
	if p, _ := hf.requests(); len(p) != 0 {
		t.Errorf("hub off made requests: %v", p)
	}
	if got := catalogRow(r, "mystery-27b").Info; got == nil || got.HubURL != "https://huggingface.co/Example-Org/Mystery-27B" || got.Params != "" {
		t.Errorf("info with the hub off: %+v", got)
	}
}

// A repo id is read from a command line only where it is clear.
func TestInferHFRepo(t *testing.T) {
	for _, tc := range [][2]string{
		{"x -m /srv/hf/hub/models--Qwen--Qwen3-4B-GGUF/snapshots/0a1b/Qwen3-4B-Q4_K_M.gguf", "Qwen/Qwen3-4B-GGUF"},
		{"x -m /srv/hub/models--example-org--Tiny-1.5B/snapshots/0a1b/model.gguf", "example-org/Tiny-1.5B"},
		{"x -m /models/Qwen3-4B-Q4_K_M.gguf", ""},
		{"x --hf-repo Example-Org/Model-GGUF:Q4_K_M", "Example-Org/Model-GGUF"},
		{"x -hfr Example-Org/Model-GGUF", "Example-Org/Model-GGUF"},
		{"vllm serve --model Qwen/Qwen3-0.6B", "Qwen/Qwen3-0.6B"},
		{"vllm serve --model=Qwen/Qwen3-0.6B", "Qwen/Qwen3-0.6B"},
		{"x --model models/qwen3.gguf", ""},
		{"x --model /srv/models/qwen3", ""},
		{"x --model ../qwen3", ""},
	} {
		if got := inferHFRepo(tc[0]); got != tc[1] {
			t.Errorf("%q: %q, want %q", tc[0], got, tc[1])
		}
	}
	for _, tc := range []struct {
		n    int64
		want string
	}{{27e9, "27B"}, {596049920, "0.6B"}, {4022468096, "4B"}, {7615616512, "7.6B"}, {135e6, "135M"}, {0, ""}} {
		if got := formatParams(tc.n); got != tc.want {
			t.Errorf("formatParams(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
