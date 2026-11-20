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

// pathAPITypes maps an API request path to the api_type that serves
// it, so a request with no model id falls back to that type's default stanza
// (config.DefaultStanza). An explicit match.path_prefix still wins.
var pathAPITypes = []struct{ prefix, apiType string }{
	{"/v1/audio/speech", config.APITypeSpeech},
	{"/v1/audio/transcriptions", config.APITypeTranscription},
	{"/v1/audio/translations", config.APITypeTranscription},
	{"/v1/realtime", config.APITypeRealtime},
	{"/v1/videos", config.APITypeVideo},
	{"/v1/audio/generations", config.APITypeAudioGen},
	{"/v1/images/", config.APITypeImage},
	{"/sdapi/v1/", config.APITypeImage},
	{"/v1/embeddings", config.APITypeEmbedding},
	{"/v1/rerank", config.APITypeRerank},
}

// ambiguousTypeError is a model-less request whose path maps to an api_type
// with several stanzas and no path_default. It answers 400, not 404: the path
// does serve a type here, so the client must name one of the candidates.
type ambiguousTypeError struct {
	apiType    string
	candidates []string
}

func (e *ambiguousTypeError) Error() string {
	return fmt.Sprintf("no model id in request and no default for api_type %q; name one of: %s",
		e.apiType, strings.Join(e.candidates, ", "))
}

// matchTypeDefault resolves a model-less request by the api_type its path
// serves. ok is false when the path maps to no type, or to a type this node
// has no stanza for; the caller then falls through to ErrNoModel.
func matchTypeDefault(path string, cfg *config.Config) (ref ModelRef, err error, ok bool) {
	for _, m := range pathAPITypes {
		if !strings.HasPrefix(path, m.prefix) {
			continue
		}
		id, cands := cfg.DefaultStanza(m.apiType)
		switch {
		case id != "":
			return ModelRef{Local: id}, nil, true
		case len(cands) > 1:
			return ModelRef{}, &ambiguousTypeError{apiType: m.apiType, candidates: cands}, true
		default:
			return ModelRef{}, nil, false
		}
	}
	return ModelRef{}, nil, false
}

// ModelRef is a resolved model reference: local stanza, pool (compat),
// peer-qualified "peer/model", or a mesh id (Raw only; peers advertise it).
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
// The body is always fully read (and restored) before matching. Model id
// extraction is one pass over those bytes plus query/form keys:
//  1. JSON body field named by each stanza's match.body_field (default
//     "model"), or query "model" for GETs / form bodies.
//  2. Stanza path_prefix match: first matching stanza in config order, unless
//     a unique path_default for that prefix was set at parse.
//  3. The api_type the path serves (pathAPITypes): its path_default stanza, or
//     its only one. Several candidates and no default is an ambiguousTypeError.
func matchRequest(r *http.Request, cfg *config.Config) (ModelRef, error) {
	body, err := readBody(r)
	if err != nil {
		return ModelRef{}, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	if m := modelFromRequest(r, body, cfg); m != "" {
		ref := resolveRef(m, cfg)
		if ref.Local == "" && ref.Pool == "" && ref.Peer == "" && !cfg.PeerAdvertises(m) {
			return ModelRef{}, fmt.Errorf("%w: %q", ErrNoModel, m)
		}
		return ref, nil
	}

	if ref, ok := matchPathPrefix(r.URL.Path, cfg); ok {
		return ref, nil
	}

	// No body model and no explicit path_prefix: fall back to the api_type
	// this path serves (speech, transcription, video, ...).
	if ref, err, ok := matchTypeDefault(r.URL.Path, cfg); ok {
		return ref, err
	}

	return ModelRef{}, ErrNoModel
}

// matchPathPrefix finds a stanza whose path_prefix is a prefix of path.
// First match in config order wins. A stanza marked path_default (unique per
// prefix at parse) wins over earlier non-default stanzas that share it.
func matchPathPrefix(path string, cfg *config.Config) (ModelRef, bool) {
	first := ""
	for _, id := range cfg.StanzaIDs() {
		s := cfg.Stanza(id)
		p := s.Match.PathPrefix
		if p == "" || !strings.HasPrefix(path, p) {
			continue
		}
		if s.Match.PathDefault {
			return ModelRef{Local: id}, true
		}
		if first == "" {
			first = id
		}
	}
	if first == "" {
		return ModelRef{}, false
	}
	return ModelRef{Local: first}, true
}

// modelFromRequest extracts a model id from the already-read body and query:
// JSON fields (per-stanza body_field), urlencoded/multipart form "model", then
// query "model" for GET and form bodies. GET and form use the "model" key,
// which body_field does not cover.
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
		if m := multipartModel(ct, body); m != "" {
			return m
		}
		return r.URL.Query().Get("model")
	}
	return ""
}

// multipartModel reads the "model" field from an already-buffered multipart
// body. matchRequest ReadAlls the request first; this only parses those bytes.
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
		part.Close()
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
