package router

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"model-router/internal/config"
)

// metadataMaxBytes caps the catalog metadata file.
const metadataMaxBytes = 1 << 20

// metadataRetry is the first retry delay after a failed fetch; it doubles per
// failure, up to refresh_seconds. Tests shorten it.
var metadataRetry = 30 * time.Second

// metadataFile is the document at catalog.metadata_url. See SPEC.md.
type metadataFile struct {
	Families map[string]metaFamily `json:"families"`
	Models   []metaModel           `json:"models"`
}

type metaFamily struct {
	Name    string `json:"name"`
	Owner   string `json:"owner"`
	IconURL string `json:"icon_url"`
}

// metaModel is one models entry. Match is a model id or a path.Match glob;
// the first entry that matches wins.
type metaModel struct {
	Match         string `json:"match"`
	Family        string `json:"family"`
	Owner         string `json:"owner"`
	Params        string `json:"params"`
	Licence       string `json:"licence"`
	ContextLength int    `json:"context_length"`
	Modality      string `json:"modality"`
	HFRepo        string `json:"hf_repo"`
}

// ModelInfo is a catalog row's detail. IconURL is the family's icon from the
// metadata file, or the hub owner's avatar when the family has no icon; the UI
// draws it only as an <img>. Quant, Size and Steps (and a non-image model's
// ContextLength) are read from a local stanza's command line. Hub and HubURL
// name and link the row's Hugging Face or ModelScope repo.
type ModelInfo struct {
	IconURL       string `json:"icon_url,omitempty"`
	Owner         string `json:"owner,omitempty"`
	Params        string `json:"params,omitempty"`
	Licence       string `json:"licence,omitempty"`
	ContextLength int    `json:"context_length,omitempty"`
	Modality      string `json:"modality,omitempty"`
	Quant         string `json:"quant,omitempty"`
	Size          string `json:"size,omitempty"`
	Steps         int    `json:"steps,omitempty"`
	Hub           string `json:"hub,omitempty"`
	HubURL        string `json:"hub_url,omitempty"`
}

func (md *metadataFile) model(id string) metaModel {
	if md != nil {
		for _, m := range md.Models {
			if ok, _ := path.Match(m.Match, id); ok {
				return m
			}
		}
	}
	return metaModel{}
}

// quantRe reads the quant from a GGUF file name, e.g. Qwen3-0.6B-Q4_K_M.gguf.
var quantRe = regexp.MustCompile(`(?i)[-_.](I?Q\d+(?:_[A-Z0-9]+)*|BF16|F16|F32)\.gguf$`)

// commandFlag is the value after the first of names in a command line, as
// "--flag value" or "--flag=value".
func commandFlag(cmd string, names ...string) string {
	f := strings.Fields(cmd)
	for i, a := range f {
		for _, n := range names {
			if a == n && i+1 < len(f) {
				return f[i+1]
			}
			if v, ok := strings.CutPrefix(a, n+"="); ok {
				return v
			}
		}
	}
	return ""
}

// catalogInfo merges a catalog row's family and detail: stanza fields win over
// the metadata file (md, nil when off), which wins over hub data, which wins
// over the built-in family table. s is nil for a remote row.
func (r *Router) catalogInfo(md *metadataFile, id string, s *config.Stanza) (*Family, *ModelInfo) {
	if s == nil {
		s = &config.Stanza{}
	}
	mm := md.model(id)
	f := modelFamily(cmp.Or(s.Family, mm.Family), id, s.Name)
	h, hub, hubURL := r.hubData(r.cfg().Catalog, md, id, s)
	info := ModelInfo{
		Owner: cmp.Or(s.Owner, mm.Owner, h.Owner), Params: cmp.Or(s.Params, mm.Params, h.Params),
		Licence: cmp.Or(mm.Licence, h.Licence), ContextLength: cmp.Or(mm.ContextLength, h.ContextLength),
		Modality: cmp.Or(mm.Modality, h.Modality), Hub: hub, HubURL: hubURL,
	}
	if f != nil && md != nil {
		if mf, ok := md.Families[strings.ToLower(f.ID)]; ok {
			f.Name, f.Owner, info.IconURL = cmp.Or(mf.Name, f.Name), cmp.Or(mf.Owner, f.Owner), mf.IconURL
		}
	}
	if info.IconURL == "" && !builtinFamily(f) {
		info.IconURL = h.IconURL
	}
	if f != nil {
		info.Owner = cmp.Or(info.Owner, f.Owner)
	}
	if s.APIType == config.APITypeImage {
		if w, h := commandFlag(s.Command, "-W", "--width"), commandFlag(s.Command, "-H", "--height"); w != "" && h != "" {
			info.Size = w + "x" + h
		}
		info.Steps, _ = strconv.Atoi(commandFlag(s.Command, "--steps"))
	} else {
		if n, _ := strconv.Atoi(commandFlag(s.Command, "--ctx-size", "-c")); n > 0 {
			info.ContextLength = n
		}
		if m := quantRe.FindStringSubmatch(commandFlag(s.Command, "--model", "-m")); m != nil {
			info.Quant = strings.ToUpper(m[1])
		}
	}
	if info == (ModelInfo{}) {
		return f, nil
	}
	return f, &info
}

// fetchMetadata GETs and parses the metadata file. On any error the caller
// keeps its last good copy.
func fetchMetadata(ctx context.Context, url string) (*metadataFile, error) {
	var md metadataFile
	if err := getJSON(ctx, url, "", 30*time.Second, &md); err != nil {
		return nil, err
	}
	return &md, nil
}

// getJSON GETs url into v: 200 only, at most metadataMaxBytes, valid JSON.
// token, when set, is sent as a Bearer token.
func getJSON(ctx context.Context, url, token string, timeout time.Duration, v any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, metadataMaxBytes+1))
	if err != nil {
		return err
	}
	if len(body) > metadataMaxBytes {
		return fmt.Errorf("body is over %d bytes", metadataMaxBytes)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// RunMetadata fetches catalog.metadata_url now and every refresh_seconds until
// ctx ends, keeping the last good copy. A run of failures logs one warning and
// backs off from metadataRetry up to refresh_seconds. It reads the live config
// each round, so a reload takes effect at the next fetch.
func (r *Router) RunMetadata(ctx context.Context) {
	fails := 0
	for {
		c := r.cfg().Catalog
		wait := time.Duration(max(c.RefreshSeconds, 1)) * time.Second
		if c.MetadataURL == "" {
			r.meta.Store(nil)
			fails = 0
		} else if md, err := fetchMetadata(ctx, c.MetadataURL); err != nil {
			if ctx.Err() != nil {
				return
			}
			if fails == 0 {
				r.logger.Warnf("catalog: metadata fetch failed, keeping the last good copy and retrying: %v", err)
			}
			fails++
			wait = min(wait, metadataRetry<<min(fails-1, 10))
		} else {
			if fails > 0 {
				r.logger.Infof("catalog: metadata fetched after %d failed tries", fails)
			}
			r.meta.Store(md)
			fails = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
