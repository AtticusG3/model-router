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
	{"/v1/audio/voices", config.APITypeSpeech},
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
// The body is read only as far as matching needs and the configured cap allows.
// JSON and form bodies are read into memory (the filters rewrite them); a
// multipart body is streamed after its model field is found, or spooled to a
// temp file when the model part comes later. A body over the cap gets a 413
// before any model is loaded. Model id extraction is one pass over those bytes
// plus query/form keys:
//  1. JSON body field named by each stanza's match.body_field (default
//     "model"), or query "model" for GETs / form bodies. On multipart, a query
//     "model" beats a model part past the first bodyPrefixBytes.
//  2. Stanza path_prefix match: first matching stanza in config order, unless
//     a unique path_default for that prefix was set at parse.
//  3. The api_type the path serves (pathAPITypes): its path_default stanza, or
//     its only one. Several candidates and no default is an ambiguousTypeError.
//
// advertised reports whether a peer serves an id that is not local.
func matchRequest(r *http.Request, cfg *config.Config, advertised func(string) bool) (ModelRef, error) {
	mb, err := readMatchBody(r, cfg.MaxRequestBodyBytes())
	if err != nil {
		return ModelRef{}, err
	}
	m, err := mb.modelID(r, cfg)
	if err != nil {
		mb.cleanup()
		return ModelRef{}, err
	}
	if m != "" {
		ref := resolveRef(m, cfg)
		if ref.Local == "" && ref.Pool == "" && ref.Peer == "" && !advertised(m) {
			mb.cleanup()
			return ModelRef{}, fmt.Errorf("%w: %q", ErrNoModel, m)
		}
		return mb.finish(r, ref, cfg)
	}
	if ref, ok := matchPathPrefix(r.URL.Path, cfg); ok {
		return mb.finish(r, ref, cfg)
	}

	// No body model and no explicit path_prefix: fall back to the api_type
	// this path serves (speech, transcription, video, ...).
	if ref, err, ok := matchTypeDefault(r.URL.Path, cfg); ok {
		if err != nil {
			mb.cleanup()
			return ModelRef{}, err
		}
		return mb.finish(r, ref, cfg)
	}

	mb.cleanup()
	return ModelRef{}, ErrNoModel
}

// matchBody is the request body after the matcher has read only what it needs:
// the whole body in memory, or a multipart prefix plus the stream after it.
type matchBody struct {
	mem    []byte        // whole body in memory (JSON, form, small body)
	pref   []byte        // multipart prefix read so far
	stream io.ReadCloser // rest of the body after pref (multipart), or the raw body
	size   int64         // declared Content-Length, or -1
	raw    bool          // non-JSON, non-multipart: never read, stream as-is
	spool  *preparedBody // body spooled to a temp file (model after the file part)
}

// readMatchBody reads the head of the body the matcher needs. ceiling is the
// largest cap any stanza accepts; a body over it is refused before reading.
func readMatchBody(r *http.Request, ceiling int64) (*matchBody, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return &matchBody{size: 0}, nil
	}
	if r.ContentLength > ceiling {
		return nil, tooLargeError(ceiling)
	}
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "multipart/form-data"):
		pref, err := io.ReadAll(io.LimitReader(r.Body, bodyPrefixBytes))
		if err != nil {
			return nil, err
		}
		mb := &matchBody{pref: pref, stream: r.Body, size: r.ContentLength}
		if len(pref) < bodyPrefixBytes {
			mb.mem, mb.pref, mb.stream = pref, nil, nil
		}
		return mb, nil
	case strings.Contains(ct, "application/json"), strings.Contains(ct, "application/x-www-form-urlencoded"):
		body, err := io.ReadAll(io.LimitReader(r.Body, ceiling+1))
		if err != nil {
			return nil, err
		}
		if int64(len(body)) > ceiling {
			return nil, tooLargeError(ceiling)
		}
		return &matchBody{mem: body, size: int64(len(body))}, nil
	default:
		// A raw body (audio, video, octet-stream): the model comes from the
		// path (or the query on a GET), so keep the stream unread.
		return &matchBody{stream: r.Body, size: r.ContentLength, raw: true}, nil
	}
}

// modelID extracts the model id from the body and query.
func (mb *matchBody) modelID(r *http.Request, cfg *config.Config) (string, error) {
	// GET and DELETE carry no request body to carry a model, so the query
	// parameter is the only place it can be (e.g. DELETE /v1/audio/voices/{id}).
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		return r.URL.Query().Get("model"), nil
	}
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "application/json"):
		return jsonModelID(mb.mem, cfg), nil
	case strings.Contains(ct, "application/x-www-form-urlencoded"):
		if vals, err := url.ParseQuery(string(mb.mem)); err == nil {
			if m := vals.Get("model"); m != "" {
				return m, nil
			}
		}
		return r.URL.Query().Get("model"), nil
	case strings.Contains(ct, "multipart/form-data"):
		var m string
		if mb.mem != nil {
			m = multipartModel(ct, bytes.NewReader(mb.mem))
		} else if m = multipartModel(ct, bytes.NewReader(mb.pref)); m == "" {
			// A query model saves spooling the whole upload, so it wins over a
			// model part that comes after the prefix.
			if q := r.URL.Query().Get("model"); q != "" {
				return q, nil
			}
			// The model part is past the prefix (usually after a file part):
			// spool the whole body and scan it from disk instead of RAM.
			var err error
			if m, err = mb.spoolAndScan(r, ct, cfg); err != nil {
				return "", err
			}
		}
		if m != "" {
			return m, nil
		}
		return r.URL.Query().Get("model"), nil
	}
	return "", nil
}

// spoolAndScan writes the rest of the body to a temp file, then reads the model
// field from it. The file becomes the body the backend receives.
func (mb *matchBody) spoolAndScan(r *http.Request, ct string, cfg *config.Config) (string, error) {
	p, err := readPrepared(io.MultiReader(bytes.NewReader(mb.pref), mb.stream), cfg.MaxRequestBodyBytes())
	if err != nil {
		return "", err
	}
	trackCleanup(r, p.cleanup)
	mb.spool, mb.mem, mb.pref, mb.stream, mb.size = p, nil, nil, nil, p.size
	f, err := p.open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	return multipartModel(ct, f), nil
}

// finish installs the prepared body on r and returns the resolved ref. limit is
// the matched stanza's cap; a body over it gets 413.
func (mb *matchBody) finish(r *http.Request, ref ModelRef, cfg *config.Config) (ModelRef, error) {
	if err := mb.install(r, cfg.RequestBodyLimit(ref.Local)); err != nil {
		mb.cleanup()
		return ModelRef{}, err
	}
	return ref, nil
}

// install leaves r.Body ready to stream the whole body to a backend, without a
// second full copy in RAM. Over limit it returns errBodyTooLarge.
func (mb *matchBody) install(r *http.Request, limit int64) error {
	if mb.spool != nil {
		if mb.spool.size > limit {
			return tooLargeError(limit)
		}
		return mb.spool.install(r)
	}
	if mb.size > limit {
		return tooLargeError(limit)
	}
	switch {
	case mb.mem != nil:
		r.Body = io.NopCloser(bytes.NewReader(mb.mem))
		r.ContentLength = int64(len(mb.mem))
	case mb.pref != nil:
		if mb.size < 0 {
			// Unknown length: bound the tail so a chunked upload over the cap
			// fails with 413 instead of running unbounded or short.
			if int64(len(mb.pref)) >= limit {
				return tooLargeError(limit)
			}
			r.Body = io.NopCloser(io.MultiReader(
				bytes.NewReader(mb.pref),
				&limitBody{rc: mb.stream, n: limit - int64(len(mb.pref)), err: tooLargeError(limit)},
			))
			return nil
		}
		// Stream the prefix already read, then the rest of the client body.
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(mb.pref), mb.stream))
		r.ContentLength = mb.size
	case mb.raw:
		// The stream is untouched; bound a chunked body as it flows.
		if mb.size < 0 {
			r.Body = &limitBody{rc: r.Body, n: limit, err: tooLargeError(limit)}
		}
	}
	return nil
}

func (mb *matchBody) cleanup() {
	if mb.spool != nil {
		mb.spool.cleanup()
	}
}

// jsonModelID reads the model id from a JSON body using each stanza's
// body_field, and always "model".
func jsonModelID(body []byte, cfg *config.Config) string {
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

// multipartModel reads the "model" field from a multipart stream. It stops at
// the model part (or the end) rather than reading the whole body, so a large
// file part before or after it stays on disk/the wire.
func multipartModel(contentType string, body io.Reader) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	boundary := params["boundary"]
	if boundary == "" {
		return ""
	}
	mr := multipart.NewReader(body, boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() == "model" {
			val, err := io.ReadAll(io.LimitReader(part, 4096))
			part.Close()
			if err != nil {
				// The value ran off the end of a prefix: not found, so the
				// caller scans the whole body rather than use a cut-off id.
				return ""
			}
			return string(val)
		}
		part.Close()
	}
}
