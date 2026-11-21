package router

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"model-router/internal/config"
)

// hubDelay is the pause before each hub request, so lookups go one at a time
// and gently. hubRetry is how often the hub loop looks for due repos and the
// first retry delay after a failed lookup (doubling, up to
// hub_refresh_seconds). Tests shorten both.
var (
	hubDelay = time.Second
	hubRetry = 30 * time.Second
)

// hubInfo is what the catalog takes from one Hugging Face or ModelScope repo.
type hubInfo struct {
	Owner         string `json:"owner,omitempty"`
	Params        string `json:"params,omitempty"`
	Licence       string `json:"licence,omitempty"`
	ContextLength int    `json:"context_length,omitempty"`
	Modality      string `json:"modality,omitempty"`
	IconURL       string `json:"icon_url,omitempty"`
}

// hubEntry is the last good lookup of one repo, keyed "<hub>:<owner>/<name>"
// in memory and in catalog.cache_file.
type hubEntry struct {
	Fetched time.Time `json:"fetched"`
	Info    hubInfo   `json:"info"`
}

// repoIDRe is an "owner/name" repo id; it also keeps ids safe in a URL path.
var repoIDRe = regexp.MustCompile(`^[A-Za-z0-9][\w.-]*/[A-Za-z0-9][\w.-]*$`)

// hfCacheRe reads the repo from a Hugging Face cache path,
// .../models--<owner>--<name>/snapshots/...
var hfCacheRe = regexp.MustCompile(`models--([A-Za-z0-9][\w.-]*?)--([A-Za-z0-9][\w.-]*)/snapshots/`)

// modelFileRe is a local weights file, which a relative "dir/file" --model
// value can look like.
var modelFileRe = regexp.MustCompile(`(?i)\.(gguf|safetensors|bin|pt|pth|onnx|ckpt)$`)

// inferHFRepo reads a Hugging Face repo id from a command line only where it
// is clear: llama.cpp's --hf-repo, a Hugging Face cache path, or a --model
// value that is an owner/name id. A bare GGUF file name maps to nothing.
func inferHFRepo(cmd string) string {
	if v, _, _ := strings.Cut(commandFlag(cmd, "--hf-repo", "-hfr"), ":"); v != "" {
		return v
	}
	if m := hfCacheRe.FindStringSubmatch(cmd); m != nil {
		return m[1] + "/" + m[2]
	}
	if v := commandFlag(cmd, "--model"); repoIDRe.MatchString(v) && !modelFileRe.MatchString(v) {
		return v
	}
	return ""
}

// hubRepos is a row's Hugging Face and ModelScope repo ids. The Hugging Face
// id is the stanza's, then the metadata file's, then one read from the command
// line. The ModelScope id is the stanza's, or the Hugging Face id when
// catalog.modelscope_fallback is on. Ids that are not owner/name are dropped.
func hubRepos(c config.Catalog, md *metadataFile, id string, s *config.Stanza) (hf, ms string) {
	hf = cmp.Or(s.HFRepo, md.model(id).HFRepo, inferHFRepo(s.Command))
	if !repoIDRe.MatchString(hf) {
		hf = ""
	}
	ms = s.ModelScopeRepo
	if ms == "" && c.ModelScopeFallback {
		ms = hf
	}
	if !repoIDRe.MatchString(ms) {
		ms = ""
	}
	return hf, ms
}

// hubData is a row's hub data and the repo link to show: Hugging Face data
// first, then ModelScope. With no data (or the hub off), it links the known repo.
func (r *Router) hubData(c config.Catalog, md *metadataFile, id string, s *config.Stanza) (h hubInfo, hub, link string) {
	hf, ms := hubRepos(c, md, id, s)
	var cache map[string]hubEntry
	if p := r.hub.Load(); p != nil {
		cache = *p
	}
	if e, ok := cache[config.HubHuggingFace+":"+hf]; ok && hf != "" && c.Hub != config.HubModelScope {
		return e.Info, "Hugging Face", "https://huggingface.co/" + hf
	}
	if e, ok := cache[config.HubModelScope+":"+ms]; ok && ms != "" && c.Hub != config.HubHuggingFace {
		return e.Info, "ModelScope", "https://modelscope.cn/models/" + ms
	}
	if hf != "" {
		return h, "Hugging Face", "https://huggingface.co/" + hf
	}
	if ms != "" {
		return h, "ModelScope", "https://modelscope.cn/models/" + ms
	}
	return h, "", ""
}

// remoteStanza stands in for a peer's stanza: only the hf_repo it advertises.
func remoteStanza(t *Telemetry, id string) *config.Stanza {
	s := &config.Stanza{}
	if t != nil {
		s.HFRepo = t.HFRepos[id]
	}
	return s
}

// localHFRepos is each local stanza's Hugging Face repo id, advertised to peers.
func (r *Router) localHFRepos() map[string]string {
	cfg, md := r.cfg(), r.meta.Load()
	out := map[string]string{}
	for _, id := range cfg.StanzaIDs() {
		if hf, _ := hubRepos(cfg.Catalog, md, id, cfg.Stanza(id)); hf != "" {
			out[id] = hf
		}
	}
	return out
}

// builtinFamily reports whether f is in the built-in table; the UI bundles an
// icon for each of them.
func builtinFamily(f *Family) bool {
	if f != nil {
		for _, e := range families {
			if e.f.ID == f.ID {
				return true
			}
		}
	}
	return false
}

// formatParams writes a parameter count as the catalog shows it: 27B, 0.6B, 135M.
func formatParams(n int64) string {
	switch {
	case n >= 5e8:
		return strconv.FormatFloat(math.Round(float64(n)/1e8)/10, 'f', -1, 64) + "B"
	case n >= 1e6:
		return strconv.FormatInt((n+5e5)/1e6, 10) + "M"
	}
	return ""
}

// firstString is v as a string, or its first element when it is a list.
func firstString(v any) string {
	if l, ok := v.([]any); ok && len(l) > 0 {
		v = l[0]
	}
	s, _ := v.(string)
	return s
}

// hubGet is one rate-limited hub request: after hubDelay, with a short timeout.
func hubGet(ctx context.Context, u, token string, v any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(hubDelay):
	}
	return getJSON(ctx, u, token, 15*time.Second, v)
}

// hfModel is the part of GET /api/models/{repo} the catalog reads.
type hfModel struct {
	Author      string `json:"author"`
	PipelineTag string `json:"pipeline_tag"`
	CardData    struct {
		License   any `json:"license"`
		BaseModel any `json:"base_model"`
	} `json:"cardData"`
	Safetensors struct {
		Total int64 `json:"total"`
	} `json:"safetensors"`
	GGUF struct {
		Total         int64 `json:"total"`
		ContextLength int   `json:"context_length"`
	} `json:"gguf"`
}

func (m *hfModel) params() string {
	return formatParams(cmp.Or(m.Safetensors.Total, m.GGUF.Total))
}

// fetchHF looks up one Hugging Face repo, its base model when it has no
// parameter count of its own, and its owner's avatar. The token from
// catalog.hub_token_env goes to Hugging Face only.
func fetchHF(ctx context.Context, c config.Catalog, repo string) (hubInfo, error) {
	token := ""
	if c.HubTokenEnv != "" {
		token = os.Getenv(c.HubTokenEnv)
	}
	var m hfModel
	if err := hubGet(ctx, c.HFBase+"/api/models/"+repo, token, &m); err != nil {
		return hubInfo{}, err
	}
	h := hubInfo{Owner: m.Author, Params: m.params(), Licence: firstString(m.CardData.License),
		ContextLength: m.GGUF.ContextLength, Modality: m.PipelineTag}
	if base := firstString(m.CardData.BaseModel); h.Params == "" && base != repo && repoIDRe.MatchString(base) {
		var b hfModel
		if hubGet(ctx, c.HFBase+"/api/models/"+base, token, &b) == nil {
			h.Params = b.params()
		}
	}
	if m.Author != "" {
		for _, kind := range []string{"organizations", "users"} {
			var a struct {
				AvatarURL string `json:"avatarUrl"`
			}
			if hubGet(ctx, c.HFBase+"/api/"+kind+"/"+url.PathEscape(m.Author)+"/avatar", token, &a) == nil && a.AvatarURL != "" {
				h.IconURL = a.AvatarURL
				if strings.HasPrefix(h.IconURL, "/") && !strings.HasPrefix(h.IconURL, "//") {
					h.IconURL = c.HFBase + h.IconURL
				}
				break
			}
		}
	}
	return h, nil
}

// fetchModelScope looks up one ModelScope repo. Fields are read loosely so an
// unexpected shape leaves that field empty instead of failing the lookup.
func fetchModelScope(ctx context.Context, c config.Catalog, repo string) (hubInfo, error) {
	var resp struct {
		Data map[string]any `json:"Data"`
	}
	if err := hubGet(ctx, c.ModelScopeBase+"/api/v1/models/"+repo, "", &resp); err != nil {
		return hubInfo{}, err
	}
	d := resp.Data
	if d == nil {
		return hubInfo{}, errors.New("no Data in the response")
	}
	h := hubInfo{Owner: firstString(d["Path"]), Licence: firstString(d["License"])}
	if tasks, _ := d["Tasks"].([]any); len(tasks) > 0 {
		if t, _ := tasks[0].(map[string]any); t != nil {
			h.Modality = firstString(t["Name"])
		}
	}
	// shortcut: the parameter count is read from ModelInfos.safetensor.total, which is not checked against a live response; confirm the field once a run has network access.
	if mi, _ := d["ModelInfos"].(map[string]any); mi != nil {
		if st, _ := mi["safetensor"].(map[string]any); st != nil {
			n, _ := st["total"].(float64)
			h.Params = formatParams(int64(n))
		}
	}
	return h, nil
}

// hubTargets is each distinct (Hugging Face, ModelScope) repo pair in the
// catalog: local stanzas, then peer rows by the hf_repo the peer advertises.
func (r *Router) hubTargets(c config.Catalog) [][2]string {
	cfg, md := r.cfg(), r.meta.Load()
	seen := map[[2]string]bool{}
	var out [][2]string
	add := func(hf, ms string) {
		k := [2]string{hf, ms}
		if (hf != "" || ms != "") && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, id := range cfg.StanzaIDs() {
		add(hubRepos(c, md, id, cfg.Stanza(id)))
	}
	for _, p := range r.PeerList() {
		t, _, _ := r.peers.Entry(p.Name)
		for _, m := range r.peerModels(p) {
			add(hubRepos(c, md, m, remoteStanza(t, m)))
		}
	}
	return out
}

// hubRun is the hub loop's per-repo failure count and next retry time.
type hubRun struct {
	fails map[string]int
	next  map[string]time.Time
}

// RunHub looks up each distinct hub repo in the catalog in the background,
// one request at a time, and again once hub_refresh_seconds has passed, until
// ctx ends. A failed lookup keeps the last good copy, logs one warning for the
// run of failures and backs off. The last good data is saved to
// catalog.cache_file, when set, and read back at start so a restart with no
// network keeps it. It reads the live config each round.
func (r *Router) RunHub(ctx context.Context) {
	run := hubRun{fails: map[string]int{}, next: map[string]time.Time{}}
	loaded := ""
	for {
		c := r.cfg().Catalog
		if c.Hub == config.HubOff {
			r.hub.Store(nil)
			loaded = ""
		} else {
			if c.CacheFile != loaded {
				loaded = c.CacheFile
				if r.hub.Load() == nil {
					r.readHubCache(c.CacheFile)
				}
			}
			for _, t := range r.hubTargets(c) {
				hf, ms := t[0], t[1]
				if hf != "" && c.Hub != config.HubModelScope {
					r.hubLookup(ctx, c, &run, config.HubHuggingFace, hf)
					if r.hubCached(config.HubHuggingFace + ":" + hf) {
						continue
					}
				}
				if ms != "" && c.Hub != config.HubHuggingFace {
					r.hubLookup(ctx, c, &run, config.HubModelScope, ms)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(hubRetry):
		}
	}
}

func (r *Router) hubCached(key string) bool {
	p := r.hub.Load()
	if p == nil {
		return false
	}
	_, ok := (*p)[key]
	return ok
}

// hubLookup fetches one repo when it is due: never fetched, or older than
// hub_refresh_seconds, and not backing off after a failure.
func (r *Router) hubLookup(ctx context.Context, c config.Catalog, run *hubRun, hub, repo string) {
	key := hub + ":" + repo
	now := time.Now()
	refresh := time.Duration(c.HubRefreshSeconds) * time.Second
	if p := r.hub.Load(); p != nil {
		if e, ok := (*p)[key]; ok && now.Sub(e.Fetched) < refresh {
			return
		}
	}
	if ctx.Err() != nil || now.Before(run.next[key]) {
		return
	}
	var h hubInfo
	var err error
	if hub == config.HubHuggingFace {
		h, err = fetchHF(ctx, c, repo)
	} else {
		h, err = fetchModelScope(ctx, c, repo)
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if run.fails[key] == 0 {
			r.logger.Warnf("catalog: %s lookup of %s failed, keeping the last good copy and retrying: %v", hub, repo, err)
		}
		run.fails[key]++
		run.next[key] = time.Now().Add(min(refresh, hubRetry<<min(run.fails[key]-1, 10)))
		return
	}
	if run.fails[key] > 0 {
		r.logger.Infof("catalog: %s lookup of %s worked after %d failed tries", hub, repo, run.fails[key])
	}
	delete(run.fails, key)
	delete(run.next, key)
	var m map[string]hubEntry
	if p := r.hub.Load(); p != nil {
		m = maps.Clone(*p)
	}
	if m == nil {
		m = map[string]hubEntry{}
	}
	m[key] = hubEntry{Fetched: time.Now(), Info: h}
	r.hub.Store(&m)
	if c.CacheFile != "" {
		if err := writeHubCache(c.CacheFile, m); err != nil {
			r.logger.Warnf("catalog: saving the hub cache: %v", err)
		}
	}
}

// readHubCache loads catalog.cache_file; a missing file is a first run.
func (r *Router) readHubCache(path string) {
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var m map[string]hubEntry
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	if err != nil {
		r.logger.Warnf("catalog: reading the hub cache: %v", err)
		return
	}
	r.hub.Store(&m)
}

// writeHubCache saves m through a temporary file so a crash never leaves half a cache.
func writeHubCache(path string, m map[string]hubEntry) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
