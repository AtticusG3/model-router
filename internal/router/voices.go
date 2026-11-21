package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"model-router/internal/config"
)

// voiceNameRe is the id shape a library voice may take; the id is also its
// two file names, so no other characters may reach the filesystem.
var voiceNameRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// voiceRecord is the JSON sidecar beside a voice's WAV clip.
type voiceRecord struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Hash     string    `json:"hash"`
	Created  time.Time `json:"created"`
	Consent  string    `json:"consent"`
	Who      string    `json:"who"`
	RefText  string    `json:"ref_text"`
	Language string    `json:"language"`
}

// voiceLibraryRoute reports a /v1/audio/voices route the library owns. A
// ?model= request still goes to that speech stanza's backend, and so does a
// POST that names its model in the body (a backend-native upload). answered
// reports that the 413 for an oversized model-less upload was written here:
// the body cannot be replayed, so ServeHTTP stops.
func (r *Router) voiceLibraryRoute(w http.ResponseWriter, req *http.Request) (library, answered bool) {
	if req.URL.Query().Get("model") != "" {
		return false, false
	}
	// A model-less collection GET is the union of library voices and backend
	// presets, not the library alone.
	if req.Method == http.MethodGet && req.URL.Path == "/v1/audio/voices" {
		return false, false
	}
	if req.Method == http.MethodPost && req.URL.Path == "/v1/audio/voices" {
		model, tooBig := r.bodyModel(req)
		if tooBig {
			http.Error(w, tooLargeError(r.cfg().MaxRequestBodyBytes()).Error(), http.StatusRequestEntityTooLarge)
			return false, true
		}
		if model != "" {
			return false, false
		}
	}
	p := req.URL.Path
	return p == "/v1/audio/voices" || strings.HasPrefix(p, "/v1/audio/voices/"), false
}

// bodyModel returns the model a POST /v1/audio/voices names in its JSON body
// or multipart form. The body is snapshotted at the request-body cap, the same
// cap the matcher applies, so a backend upload is not limited by the library
// clip cap; it is then installed back on the request unchanged. tooBig reports
// a body over the request-body cap.
func (r *Router) bodyModel(req *http.Request) (model string, tooBig bool) {
	ct := req.Header.Get("Content-Type")
	if !strings.Contains(ct, "multipart/form-data") && !strings.Contains(ct, "application/json") {
		return "", false
	}
	cfg := r.cfg()
	if req.ContentLength > cfg.MaxRequestBodyBytes() {
		return "", true
	}
	p, err := r.snapshotBody(req, cfg.MaxRequestBodyBytes())
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			return "", true
		}
		// shortcut: another read error leaves the body partly consumed, so a
		// model-less upload is answered by the library; spool it like the matcher
		// does if clients can recover mid-upload.
		return "", false
	}
	// Scan a separate reader so the body snapshotBody installed stays unread.
	rc, err := p.open()
	if err != nil {
		return "", false
	}
	defer rc.Close()
	if strings.Contains(ct, "multipart/form-data") {
		return multipartModel(ct, rc), false
	}
	raw, err := io.ReadAll(rc)
	if err != nil {
		return "", false
	}
	return jsonModelID(raw, cfg), false
}

// serveVoiceLibrary answers the library's own voice routes. It runs before
// matching, so a configured library never starts a speech backend just to
// store or list a voice.
func (r *Router) serveVoiceLibrary(w http.ResponseWriter, req *http.Request) {
	started := time.Now()
	if req.URL.Path == "/v1/audio/voices" {
		switch req.Method {
		case http.MethodPost:
			r.createLibraryVoice(w, req, started)
		default:
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	id := strings.TrimPrefix(req.URL.Path, "/v1/audio/voices/")
	if id == "" || !voiceNameRe.MatchString(id) {
		http.Error(w, "voice not found", http.StatusNotFound)
		return
	}
	switch req.Method {
	case http.MethodGet:
		r.getLibraryVoice(w, id)
	case http.MethodDelete:
		r.deleteLibraryVoice(w, req, id, started)
	default:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *Router) getLibraryVoice(w http.ResponseWriter, id string) {
	rec, ok, err := r.readVoiceRecord(id)
	if err != nil {
		http.Error(w, "read voice: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "voice not found", http.StatusNotFound)
		return
	}
	writeJSON(w, rec)
}

func (r *Router) createLibraryVoice(w http.ResponseWriter, req *http.Request, started time.Time) {
	cfg := r.cfg()
	maxBytes := cfg.VoiceMaxBytes()
	// The clip cap plus room for the multipart envelope; a body far over it
	// is refused before it is read into memory.
	req.Body = http.MaxBytesReader(w, req.Body, maxBytes+1<<20)
	if !strings.Contains(req.Header.Get("Content-Type"), "multipart/form-data") {
		http.Error(w, "voice upload must be multipart/form-data", http.StatusBadRequest)
		return
	}
	if err := req.ParseMultipartForm(maxBytes); err != nil {
		if req.MultipartForm != nil {
			_ = req.MultipartForm.RemoveAll()
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "voice clip exceeds the size cap", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "parse voice upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer req.MultipartForm.RemoveAll()

	name := strings.TrimSpace(req.FormValue("name"))
	consent := strings.TrimSpace(req.FormValue("consent"))
	refText := strings.TrimSpace(req.FormValue("ref_text"))
	language := strings.TrimSpace(req.FormValue("language"))
	// Consuming a voice without consent is what the library exists to stop.
	if name == "" || consent == "" || refText == "" || language == "" {
		http.Error(w, "name, file, ref_text, language and consent are required", http.StatusBadRequest)
		return
	}
	if !voiceNameRe.MatchString(name) {
		http.Error(w, "name must match [a-z0-9-]{1,64}", http.StatusBadRequest)
		return
	}
	file, _, err := req.FormFile("file")
	if err != nil {
		// Accept the OpenAI custom-voice field name as well.
		file, _, err = req.FormFile("audio_sample")
	}
	if err != nil {
		http.Error(w, "file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()
	clip, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		http.Error(w, "read voice clip: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(clip)) > maxBytes {
		http.Error(w, "voice clip exceeds the size cap", http.StatusRequestEntityTooLarge)
		return
	}
	seconds := wavSeconds(clip, int64(len(clip)))
	if seconds <= 0 {
		http.Error(w, "file must be a WAV clip", http.StatusBadRequest)
		return
	}
	if capSeconds := float64(cfg.VoiceMaxSecondsValue()); seconds > capSeconds {
		http.Error(w, fmt.Sprintf("voice clip is %.1fs, over the %.0fs cap", seconds, capSeconds), http.StatusRequestEntityTooLarge)
		return
	}

	rec := voiceRecord{
		ID:       name,
		Name:     name,
		Hash:     fmt.Sprintf("%x", sha256.Sum256(clip)),
		Created:  time.Now().UTC(),
		Consent:  consent,
		Who:      req.RemoteAddr,
		RefText:  refText,
		Language: language,
	}
	if code, msg := r.writeVoice(rec, clip); code != 0 {
		http.Error(w, msg, code)
		return
	}
	r.logVoiceAdmin(req, http.StatusCreated, rec.ID, started)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, rec)
}

func (r *Router) deleteLibraryVoice(w http.ResponseWriter, req *http.Request, id string, started time.Time) {
	r.voiceMu.Lock()
	dir := r.cfg().VoiceDir
	rec, ok, err := readVoiceRecord(dir, id)
	if err == nil && ok {
		if err = os.Remove(filepath.Join(dir, id+".wav")); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	if err == nil && ok {
		err = os.Remove(filepath.Join(dir, id+".json"))
	}
	r.voiceMu.Unlock()
	if err != nil {
		http.Error(w, "delete voice: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "voice not found", http.StatusNotFound)
		return
	}
	r.logVoiceAdmin(req, http.StatusOK, id, started)
	writeJSON(w, map[string]any{"id": rec.ID, "deleted": true})
}

// writeVoice stores the clip and its sidecar under the configured voice_dir.
// A non-zero status code means the caller should answer with it.
func (r *Router) writeVoice(rec voiceRecord, clip []byte) (int, string) {
	r.voiceMu.Lock()
	defer r.voiceMu.Unlock()

	dir := r.cfg().VoiceDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return http.StatusInternalServerError, "create voice_dir: " + err.Error()
	}
	if _, ok, err := readVoiceRecord(dir, rec.ID); err != nil {
		return http.StatusInternalServerError, err.Error()
	} else if ok {
		return http.StatusConflict, "voice already exists: " + rec.ID
	}

	clipPath := filepath.Join(dir, rec.ID+".wav")
	sidecar, err := json.Marshal(rec)
	if err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	if err := writeFileAtomic(clipPath, clip); err != nil {
		return http.StatusInternalServerError, "write voice clip: " + err.Error()
	}
	if err := writeFileAtomic(filepath.Join(dir, rec.ID+".json"), sidecar); err != nil {
		_ = os.Remove(clipPath)
		return http.StatusInternalServerError, "write voice record: " + err.Error()
	}
	return 0, ""
}

// writeFileAtomic writes data through a temp file in the same directory, so a
// reader never sees half a voice after a crash or a concurrent delete.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".voice-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (r *Router) voiceRecords() ([]voiceRecord, error) {
	dir := r.cfg().VoiceDir
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []voiceRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]voiceRecord, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !voiceNameRe.MatchString(id) {
			continue
		}
		if rec, ok, err := readVoiceRecord(dir, id); err == nil && ok {
			out = append(out, rec)
		}
	}
	slices.SortFunc(out, func(a, b voiceRecord) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (r *Router) readVoiceRecord(id string) (voiceRecord, bool, error) {
	return readVoiceRecord(r.cfg().VoiceDir, id)
}

func readVoiceRecord(dir, id string) (voiceRecord, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return voiceRecord{}, false, nil
	}
	if err != nil {
		return voiceRecord{}, false, err
	}
	var rec voiceRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return voiceRecord{}, false, fmt.Errorf("voice %s: %w", id, err)
	}
	return rec, true, nil
}

// injectLibraryVoice replaces a named library voice with the reference fields
// the stanza's clone_mode expects: a data URI plus ref_text, or raw base64
// plus lang. A client-supplied reference is left alone, and an unknown name is
// forwarded as the backend's own voice (a stock preset, for example).
func (r *Router) injectLibraryVoice(req *http.Request, s *config.Stanza) error {
	if req.Body == nil || !strings.Contains(req.Header.Get("Content-Type"), "application/json") {
		return nil
	}
	// No library means the backend owns every voice name, and a name that is
	// not a library id may not reach the filesystem at all.
	dir := r.cfg().VoiceDir
	if dir == "" {
		return nil
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil
	}
	if _, ok := obj["ref_audio"]; ok {
		return nil
	}
	if _, ok := obj["speaker_ref_b64"]; ok {
		return nil
	}
	var name string
	if json.Unmarshal(obj["voice"], &name) != nil || !voiceNameRe.MatchString(name) {
		return nil
	}
	rec, ok, err := readVoiceRecord(dir, name)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	clip, err := os.ReadFile(filepath.Join(dir, name+".wav"))
	if err != nil {
		return fmt.Errorf("voice %s: %w", name, err)
	}
	b64 := base64.StdEncoding.EncodeToString(clip)
	mode := ""
	if s != nil {
		mode = s.CloneMode
	}
	if mode == config.CloneModeSpeakerRefB64 {
		return applyFilters(req, nil, []string{"voice"}, map[string]any{
			"speaker_ref_b64": b64,
			"lang?":           rec.Language,
		})
	}
	return applyFilters(req, nil, []string{"voice"}, map[string]any{
		"ref_audio": "data:audio/wav;base64," + b64,
		"ref_text":  rec.RefText,
		"language?": rec.Language,
	})
}

// speechStanzaFor picks the clone_mode used when a peer (which does not
// publish its stanzas) receives a resolved voice: the local stanza with that
// id when there is one, else the speech default. nil means the built-in
// ref_audio shape.
func (r *Router) speechStanzaFor(id string) *config.Stanza {
	cfg := r.cfg()
	if s := cfg.Stanza(id); s != nil && s.APIType == config.APITypeSpeech {
		return s
	}
	if def, _ := cfg.DefaultStanza(config.APITypeSpeech); def != "" {
		return cfg.Stanza(def)
	}
	return nil
}

// logVoiceAdmin records a voice write or delete as an admin row. The clip
// itself is never kept in the row.
func (r *Router) logVoiceAdmin(req *http.Request, status int, id string, started time.Time) {
	if r.activity == nil {
		return
	}
	r.activity.record(ActivityEntry{
		Timestamp:  started,
		Method:     req.Method,
		Path:       req.URL.Path,
		Status:     status,
		DurationMs: int(time.Since(started).Milliseconds()),
		Media:      &MediaStats{Kind: voiceAdminKind, Voice: id},
	}, ActivityCapture{}, nil, "", [2][]preview{})
}
