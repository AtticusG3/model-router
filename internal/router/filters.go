package router

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"model-router/internal/config"
)

// applyFilters strips then sets top-level keys on a JSON object body. Later
// maps in sets win. A key ending in "?" is set only if the client's body
// (after stripping) omitted it. Anything that is not a JSON object body is
// left untouched. The matcher already buffered r.Body.
func applyFilters(req *http.Request, strip []string, sets ...map[string]any) error {
	n := len(strip)
	for _, set := range sets {
		n += len(set)
	}
	if n == 0 {
		return nil // no filters: do not re-encode the client's body
	}
	if req.Body == nil || !strings.Contains(req.Header.Get("Content-Type"), "application/json") {
		return nil
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	// RawMessage values keep untouched fields byte-exact (no float rounding).
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil
	}
	for _, k := range strip {
		delete(obj, k)
	}
	client := map[string]bool{}
	for k := range obj {
		client[k] = true
	}
	for _, set := range sets {
		for k, v := range set {
			bare := strings.TrimSuffix(k, "?")
			if bare != k && client[bare] {
				continue
			}
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			obj[bare] = b
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(out))
	req.ContentLength = int64(len(out))
	return nil
}

// filterLocal applies the stanza's filters for the id the client used (a
// setParamsByID alias, or the stanza id / empty).
func filterLocal(req *http.Request, s *config.Stanza) error {
	requested, _ := req.Context().Value(requestedIDKey{}).(string)
	f := &s.Filters
	return applyFilters(req, f.StripParams, f.SetParams, f.SetParamsByID[requested])
}

// requestedIDKey carries the model id the client named, so a setParamsByID
// alias still selects its parameter set after routing has picked a stanza.
type requestedIDKey struct{}

func withRequestedID(req *http.Request, id string) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), requestedIDKey{}, id))
}
