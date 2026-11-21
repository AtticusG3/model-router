package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"

	"model-router/internal/config"
)

// applyFilters renames, strips then sets top-level keys on a JSON object body,
// or form fields on a urlencoded or multipart body (a rename may move a file
// part; strip and set never touch one). Later maps in sets win. A key ending
// in "?" is set only if the client's body (after stripping) omitted it. Any
// other body is left untouched.
func applyFilters(req *http.Request, rename map[string]string, strip []string, sets ...map[string]any) error {
	n := len(rename) + len(strip)
	for _, set := range sets {
		n += len(set)
	}
	if n == 0 {
		return nil // no filters: do not re-encode the client's body
	}
	if req.Body == nil {
		return nil
	}
	if !strings.Contains(req.Header.Get("Content-Type"), "application/json") {
		e, err := newFormEdit(rename, strip, sets...)
		if err != nil {
			return err
		}
		return editForm(req, e)
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
	// Build the renames from the original body before deleting anything, so a
	// chain (a→b, b→c) or a swap gives one result whatever order the map is
	// walked in. An identity rename leaves the field where it is.
	moved := map[string]json.RawMessage{}
	var drop []string
	for from, to := range rename {
		if from == to {
			continue
		}
		if v, ok := obj[from]; ok {
			moved[to] = v
			drop = append(drop, from)
		}
	}
	for _, k := range drop {
		delete(obj, k)
	}
	maps.Copy(obj, moved)
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

// formEdit is a set of form field changes: rename moves a field (or file part)
// to the backend's name, strip drops fields, set gives the value each named
// field ends up with.
type formEdit struct {
	rename map[string]string
	strip  map[string]bool
	set    map[string]*formSet
}

// formSet is one field to set: force replaces the client's value, fallback (a
// "?" key) fills the field only when the client omitted it.
type formSet struct{ force, fallback *string }

// value is what the field ends up as; ok false keeps the client's value.
func (s *formSet) value(present bool) (v string, ok bool) {
	switch {
	case present && s.force == nil:
		return "", false
	case present:
		return *s.force, true
	case s.fallback != nil:
		return *s.fallback, true
	}
	return *s.force, true
}

// newFormEdit turns filters into a formEdit with the same precedence as the
// JSON path: strip first, later sets win, "?" keys yield to the client.
func newFormEdit(rename map[string]string, strip []string, sets ...map[string]any) (formEdit, error) {
	e := formEdit{rename: rename, strip: map[string]bool{}, set: map[string]*formSet{}}
	for _, k := range strip {
		e.strip[k] = true
	}
	for _, set := range sets {
		for k, v := range set {
			s, ok := v.(string)
			if !ok {
				b, err := json.Marshal(v) // numbers and bools read the same in a form
				if err != nil {
					return e, err
				}
				s = string(b)
			}
			bare := strings.TrimSuffix(k, "?")
			fs := e.set[bare]
			if fs == nil {
				fs = &formSet{}
				e.set[bare] = fs
			}
			if bare != k {
				fs.fallback = &s
			} else {
				fs.force, fs.fallback = &s, nil
			}
		}
	}
	return e, nil
}

// editForm applies e to a urlencoded or multipart body; any other body is left
// alone. A urlencoded body is small (the matcher held it in memory); a
// multipart body is streamed part by part so a file part is never buffered.
func editForm(req *http.Request, e formEdit) error {
	if req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	mt, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		return nil
	}
	switch mt {
	case "application/x-www-form-urlencoded":
		raw, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return err
		}
		vals, err := url.ParseQuery(string(raw))
		if err != nil {
			req.Body = io.NopCloser(bytes.NewReader(raw)) // not a form we can edit: forward as sent
			return nil
		}
		// Same order as the JSON branch: build from the original values, then
		// delete the sources, so a rename chain or swap is stable and an
		// identity rename keeps the field.
		moved := url.Values{}
		var drop []string
		for from, to := range e.rename {
			if from == to {
				continue
			}
			if vals.Has(from) {
				moved[to] = vals[from]
				drop = append(drop, from)
			}
		}
		for _, k := range drop {
			vals.Del(k)
		}
		for k, v := range moved {
			vals[k] = v
		}
		for k := range e.strip {
			vals.Del(k)
		}
		for k, s := range e.set {
			if v, ok := s.value(vals.Has(k)); ok {
				vals.Set(k, v)
			}
		}
		out := vals.Encode()
		req.Body = io.NopCloser(strings.NewReader(out))
		req.ContentLength = int64(len(out))
	case "multipart/form-data":
		editMultipart(req, params["boundary"], e)
	}
	return nil
}

// editMultipart re-streams a multipart body through e with the same boundary,
// so the Content-Type header stays right for a retry that reinstalls the
// original body. The new length is unknown, so the body goes out chunked.
// shortcut: an edited multipart body is always chunked, work out its length up front if a backend refuses chunked uploads.
func editMultipart(req *http.Request, boundary string, e formEdit) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	if boundary == "" || mw.SetBoundary(boundary) != nil {
		return // not an RFC 2046 boundary, so no valid client sent it: forward as sent
	}
	src := &srcReader{ReadCloser: req.Body}
	// A body nobody reads (an error before the proxy) must not pin the goroutine.
	ctx := req.Context()
	stop := context.AfterFunc(ctx, func() { pr.CloseWithError(ctx.Err()) })
	go func() {
		defer stop()
		defer src.Close()
		err := copyParts(multipart.NewReader(src, boundary), mw, e)
		// The source's own error (a body over the cap) reaches the proxy as is;
		// anything else is a body that is not valid multipart.
		if err != nil && src.err == nil {
			err = fmt.Errorf("%w: %v", errBadMultipart, err)
		}
		pw.CloseWithError(err)
	}()
	req.Body, req.ContentLength = pr, -1
}

// errBadMultipart marks a multipart body that broke partway through being
// re-streamed (a bad part header, no closing boundary). Part of it has already
// gone upstream, so it cannot be resent as is; proxyTo answers 400, not 502.
var errBadMultipart = errors.New("malformed multipart body")

// srcReader remembers the body's own read error, so copyParts' parse errors
// can be told apart from it.
type srcReader struct {
	io.ReadCloser
	err error
}

func (s *srcReader) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}

// copyParts copies every part from mr to mw, applying e to text fields. Part
// headers are kept; part bodies are copied raw, never decoded.
func copyParts(mr *multipart.Reader, mw *multipart.Writer, e formEdit) error {
	seen := map[string]bool{}
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, value, replace := p.FormName(), "", false
		hdr := p.Header
		if to, ok := e.rename[name]; ok {
			if hdr, err = renamePart(hdr, to); err != nil {
				return err
			}
			name = to
		}
		if p.FileName() == "" {
			if e.strip[name] {
				continue
			}
			if s := e.set[name]; s != nil {
				value, replace = s.value(true)
				if replace && seen[name] {
					continue // a repeated field being replaced is sent once
				}
				seen[name] = true
			}
		}
		w, err := mw.CreatePart(hdr)
		if err != nil {
			return err
		}
		if replace {
			_, err = io.WriteString(w, value)
		} else {
			_, err = io.Copy(w, p)
		}
		if err != nil {
			return err
		}
	}
	for _, k := range slices.Sorted(maps.Keys(e.set)) {
		if !seen[k] {
			v, _ := e.set[k].value(false)
			if err := mw.WriteField(k, v); err != nil {
				return err
			}
		}
	}
	return mw.Close()
}

// renamePart returns a copy of a part header whose Content-Disposition names
// the field to; the filename and other parameters are kept.
func renamePart(h textproto.MIMEHeader, to string) (textproto.MIMEHeader, error) {
	disp, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
	if err != nil {
		return nil, err
	}
	params["name"] = to
	out := maps.Clone(h)
	out.Set("Content-Disposition", mime.FormatMediaType(disp, params))
	return out, nil
}

// filterLocal applies the stanza's filters for the id the client used (a
// setParamsByID alias, or the stanza id / empty).
func filterLocal(req *http.Request, s *config.Stanza) error {
	requested, _ := req.Context().Value(requestedIDKey{}).(string)
	f := &s.Filters
	return applyFilters(req, f.RenameParams, f.StripParams, f.SetParams, f.SetParamsByID[requested])
}

// requestedIDKey carries the model id the client named, so a setParamsByID
// alias still selects its parameter set after routing has picked a stanza.
type requestedIDKey struct{}

func withRequestedID(req *http.Request, id string) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), requestedIDKey{}, id))
}
