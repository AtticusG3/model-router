package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"model-router/internal/config"
)

// ErrNoModel is returned when a request cannot be resolved to any model.
// The message mirrors llama-swap's so existing clients keep their behaviour.
var ErrNoModel = errors.New("no model id could be identified")

// ModelRef is a resolved model reference. It is either a local stanza id, a
// pool name, or a peer-qualified "peer/model" id.
type ModelRef struct {
	Raw    string // as requested, e.g. "coding-pool", "digger/coding-model"
	Local  string // local stanza id ("" if not local)
	Pool   string // pool name ("" if not a pool)
	Peer   string // peer name ("" if not peer-qualified)
	PeerID string // model id within the peer ("" if not peer-qualified)
}

// resolveRef splits a raw model id into its parts.
func resolveRef(raw string, cfg *config.Config) ModelRef {
	ref := ModelRef{Raw: raw}
	if strings.Contains(raw, "/") {
		parts := strings.SplitN(raw, "/", 2)
		if cfg.Peer(parts[0]) != nil {
			ref.Peer = parts[0]
			ref.PeerID = parts[1]
			return ref
		}
		return ref // unknown qualified id; leave empty so lookup fails
	}
	if cfg.Stanza(raw) != nil {
		ref.Local = raw
		return ref
	}
	if cfg.Pool(raw) != nil {
		ref.Pool = raw
		return ref
	}
	if s := cfg.Alias(raw); s != nil {
		ref.Local = s.ModelID
		return ref
	}
	return ref
}

// matchRequest resolves an incoming request to a ModelRef.
//
// Order (llama-swap compatible, then SPEC's path_prefix):
//  1. JSON body field named by each stanza's match.body_field (default
//     "model"), or query "model" for GETs / form bodies.
//  2. Stanza path_prefix match; if several stanzas share the prefix, the one
//     marked path_default wins; otherwise the first declared.
func matchRequest(r *http.Request, cfg *config.Config) (ModelRef, error) {
	body, err := readBody(r)
	if err != nil {
		return ModelRef{}, err
	}
	// Restore the body for downstream proxying.
	r.Body = io.NopCloser(bytes.NewReader(body))

	// 1. body / query / form model field.
	if m := modelFromRequest(r, body, cfg); m != "" {
		ref := resolveRef(m, cfg)
		if ref.Local == "" && ref.Pool == "" && ref.Peer == "" {
			return ModelRef{}, fmt.Errorf("%w: %q", ErrNoModel, m)
		}
		return ref, nil
	}

	// 2. path prefix.
	if ref, ok := matchPathPrefix(r.URL.Path, cfg); ok {
		return ref, nil
	}

	return ModelRef{}, ErrNoModel
}

// matchPathPrefix finds a stanza whose path_prefix is a prefix of path.
func matchPathPrefix(path string, cfg *config.Config) (ModelRef, bool) {
	best := ""
	bestDefault := false
	for _, id := range cfg.StanzaIDs() {
		s := cfg.Stanza(id)
		p := s.Match.PathPrefix
		if p == "" {
			continue
		}
		if strings.HasPrefix(path, p) {
			// Prefer a path_default stanza over any plain prefix match.
			if s.Match.PathDefault {
				return ModelRef{Local: id}, true
			}
			if !bestDefault {
				best = id
			}
		}
	}
	if best != "" {
		return ModelRef{Local: best}, true
	}
	return ModelRef{}, false
}

// modelFromRequest extracts a model id from the request body/query/form,
// mirroring llama-swap's extractContext: JSON body (per-stanza body_field),
// urlencoded/multipart form "model", then query "model". GET and form requests
// use the "model" key, which body_field does not cover.
func modelFromRequest(r *http.Request, body []byte, cfg *config.Config) string {
	if r.Method == http.MethodGet {
		return r.URL.Query().Get("model")
	}
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			return ""
		}
		for _, field := range cfg.BodyFields() {
			if m, ok := obj[field].(string); ok && m != "" {
				return m
			}
		}
		return ""
	}
	if strings.Contains(ct, "application/x-www-form-urlencoded") {
		if vals, err := url.ParseQuery(string(body)); err == nil {
			if m := vals.Get("model"); m != "" {
				return m
			}
		}
		return r.URL.Query().Get("model")
	}
	if strings.Contains(ct, "multipart/form-data") {
		if m := multipartModel(r.Header.Get("Content-Type"), body); m != "" {
			return m
		}
		return r.URL.Query().Get("model")
	}
	return ""
}

// multipartModel extracts the "model" form field from a multipart body without
// buffering non-model parts (image uploads can be large). It returns "" when
// the field is absent or the body is not a well-formed multipart payload.
func multipartModel(contentType string, body []byte) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	boundary := params["boundary"]
	if boundary == "" {
		return ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() == "model" {
			val, _ := io.ReadAll(io.LimitReader(part, 4096))
			part.Close()
			return string(val)
		}
		part.Close() // drain and skip this part
	}
}

// readBody reads the request body fully. matchRequest restores r.Body
// afterwards so downstream proxying can re-read it.
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(r.Body)
}
