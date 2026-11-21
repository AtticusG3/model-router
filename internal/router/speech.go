package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"model-router/internal/config"
)

// takeSpeechSlot waits until instance key has fewer than slots speech requests
// in its backend, then holds one until release. A speech backend batches up to
// its own limit, so the router queues the rest here instead of piling them
// onto the backend. The wait shares the request's admit budget
// (admit_timeout_seconds); running out is an admitError, a 503 with
// Retry-After.
func (r *Router) takeSpeechSlot(ctx context.Context, key string, slots int) (release func(), err error) {
	deadline := time.Now().Add(r.cfg().AdmitTimeout())
	if d, ok := ctx.Value(admitDeadlineKey{}).(time.Time); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		r.mu.Lock()
		if r.speaking[key] < slots {
			r.speaking[key]++
			r.mu.Unlock()
			return func() {
				r.mu.Lock()
				r.speaking[key]--
				r.mu.Unlock()
			}, nil
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			return nil, &admitError{fmt.Sprintf("timed out waiting for a free slot on %s (all %d busy)", key, slots)}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// isSynthesis is a request that makes speech (one input or a batch). Voice
// list and upload requests take no synthesis slot.
func isSynthesis(req *http.Request) bool {
	return req.Method == http.MethodPost && strings.HasPrefix(req.URL.Path, "/v1/audio/speech")
}

// isVoiceList is a model-less GET /v1/audio/voices, the one request the router
// may answer from several speech stanzas.
func isVoiceList(req *http.Request) bool {
	return req.Method == http.MethodGet && req.URL.Path == "/v1/audio/voices" && req.URL.Query().Get("model") == ""
}

// serveVoiceUnion answers a model-less voice list from more than one speech
// stanza: the type default (loaded if needed) and every other speech stanza
// already running, so a voice list never starts or evicts a second model. Each
// voice carries its stanza's model id. False (nothing written) leaves one
// stanza or none to the normal route.
func (r *Router) serveVoiceUnion(w http.ResponseWriter, req *http.Request) bool {
	cfg := r.cfg()
	voices := []any{}
	// The library joins the model-less list rather than replacing it: the
	// backend presets stay visible, each library row is tagged with source.
	if cfg.VoiceDir != "" {
		recs, err := r.voiceRecords()
		if err != nil {
			r.logger.Warnf("voice library unavailable: %v", err)
		}
		for _, rec := range recs {
			voices = append(voices, libraryVoiceRow{voiceRecord: rec, Source: "library"})
		}
	}
	def, _ := cfg.DefaultStanza(config.APITypeSpeech)
	var ids []string
	for _, id := range cfg.StanzaIDs() {
		if s := cfg.Stanza(id); s.APIType == config.APITypeSpeech && (id == def || r.anyRunning(s)) {
			ids = append(ids, id)
		}
	}
	if len(ids) < 2 {
		if cfg.VoiceDir == "" {
			return false
		}
		// With a library, the one candidate stanza's list joins the union
		// instead of the library answering alone; a stanza that does not list
		// is left out and the library rows answer by themselves.
		if len(ids) == 1 {
			if list, err := r.stanzaVoices(req, ids[0]); err == nil {
				voices = append(voices, list...)
			} else {
				r.logger.Warnf("voice list left out %s: %v", ids[0], err)
			}
		}
		writeJSON(w, map[string]any{"object": "list", "data": voices})
		return true
	}
	var failed []string
	for _, id := range ids {
		list, err := r.stanzaVoices(req, id)
		if err != nil {
			failed = append(failed, id+": "+err.Error())
			continue
		}
		voices = append(voices, list...)
	}
	if len(failed) == len(ids) && cfg.VoiceDir == "" {
		http.Error(w, "no speech backend listed its voices: "+strings.Join(failed, "; "), http.StatusBadGateway)
		return true
	}
	if len(failed) > 0 {
		r.logger.Warnf("voice list left out %s", strings.Join(failed, "; "))
	}
	writeJSON(w, map[string]any{"object": "list", "data": voices})
	return true
}

// libraryVoiceRow is a library record as a list row, distinguishable from a
// backend voice by its source field.
type libraryVoiceRow struct {
	voiceRecord
	Source string `json:"source"`
}

// stanzaVoices fetches one stanza's voice list through the normal local path
// (load, slots, path_rewrite) and tags each voice with the model id. The list
// is the answer's "data" or "voices" array, or the answer itself; a voice given
// as a bare string becomes {"id": ...}.
func (r *Router) stanzaVoices(req *http.Request, id string) ([]any, error) {
	sub := req.Clone(req.Context())
	q := sub.URL.Query()
	q.Set("model", id)
	sub.URL.RawQuery = q.Encode()
	rec := httptest.NewRecorder()
	if err := r.proxyLocal(rec, sub, id); err != nil {
		return nil, err
	}
	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", rec.Code)
	}
	var v any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	items, _ := v.([]any)
	if m, ok := v.(map[string]any); ok {
		if items, _ = m["data"].([]any); items == nil {
			items, _ = m["voices"].([]any)
		}
	}
	if items == nil {
		return nil, fmt.Errorf("no voice list in the answer")
	}
	for i, item := range items {
		switch it := item.(type) {
		case string:
			items[i] = map[string]any{"id": it, "model": id}
		case map[string]any:
			it["model"] = id
		}
	}
	return items, nil
}

// anyRunning reports whether any instance of s is up and serving.
func (r *Router) anyRunning(s *config.Stanza) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for n := range s.InstanceCount() {
		if m := r.managed[instanceKey(s.ModelID, n)]; m != nil && m.State() == StateRunning {
			return true
		}
	}
	return false
}
