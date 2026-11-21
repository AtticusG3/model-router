package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-router/internal/config"
)

func voiceUpload(t *testing.T, fields map[string]string, clip []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if clip != nil {
		f, err := w.CreateFormFile("file", "clip.wav")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(clip); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

func postVoice(t *testing.T, h http.Handler, fields map[string]string, clip []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := voiceUpload(t, fields, clip)
	req := httptest.NewRequest("POST", "/v1/audio/voices", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func voiceFields() map[string]string {
	return map[string]string{
		"name":     "narrator-a",
		"consent":  "consent-0001",
		"ref_text": "A fictional line recorded for this test.",
		"language": "English",
	}
}

// Register, list, fetch and delete a voice; the clip and sidecar appear and
// disappear under voice_dir, and each write leaves an admin activity row.
func TestVoiceLibraryRegisterListDelete(t *testing.T) {
	dir := t.TempDir()
	r, h := speechNode(t, "voice_dir: "+dir+"\nstanzas:"+speechStanza("tts-main", ""), fakeSpeech())

	clip := activityWAV(1600)
	rec := postVoice(t, h, voiceFields(), clip)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST status %d body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("POST Content-Type = %q", ct)
	}
	for _, name := range []string{"narrator-a.wav", "narrator-a.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "narrator-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	var side voiceRecord
	if err := json.Unmarshal(raw, &side); err != nil {
		t.Fatal(err)
	}
	if side.ID != "narrator-a" || side.Consent != "consent-0001" || side.RefText == "" || side.Language != "English" || side.Hash == "" || side.Who == "" {
		t.Fatalf("sidecar = %+v", side)
	}

	list := serve(h, "GET", "/v1/audio/voices", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"id":"narrator-a"`) {
		t.Fatalf("list: status %d body %s", list.Code, list.Body.String())
	}
	one := serve(h, "GET", "/v1/audio/voices/narrator-a", "")
	if one.Code != http.StatusOK || !strings.Contains(one.Body.String(), `"language":"English"`) {
		t.Fatalf("get: status %d body %s", one.Code, one.Body.String())
	}
	rows := r.Activity().List()
	if len(rows) != 1 || rows[0].Media == nil || rows[0].Media.Kind != voiceAdminKind || rows[0].Media.Voice != "narrator-a" || rows[0].Status != http.StatusCreated {
		t.Fatalf("activity = %+v", rows)
	}

	del := serve(h, "DELETE", "/v1/audio/voices/narrator-a", "")
	if del.Code != http.StatusOK {
		t.Fatalf("DELETE status %d body %s", del.Code, del.Body.String())
	}
	for _, name := range []string{"narrator-a.wav", "narrator-a.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still present, err %v", name, err)
		}
	}
	if list = serve(h, "GET", "/v1/audio/voices", ""); strings.Contains(list.Body.String(), `"id":"narrator-a"`) {
		t.Fatalf("deleted voice still listed: %s", list.Body.String())
	}
	if rows = r.Activity().List(); len(rows) != 2 || rows[0].Method != http.MethodDelete || rows[0].Media.Kind != voiceAdminKind {
		t.Fatalf("activity after delete = %+v", rows)
	}
}

// A missing consent or a clip over either cap is refused before anything is
// written.
func TestVoiceLibraryRejectsMissingConsentAndCaps(t *testing.T) {
	dir := t.TempDir()
	_, h := speechNode(t, "voice_dir: "+dir+"\nstanzas:"+speechStanza("tts-main", ""), fakeSpeech())

	fields := voiceFields()
	delete(fields, "consent")
	if rec := postVoice(t, h, fields, activityWAV(1600)); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing consent: status %d body %s", rec.Code, rec.Body.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a refused upload wrote %d files", len(entries))
	}
	long := activityWAV(8000*61 + 44)
	if rec := postVoice(t, h, voiceFields(), long); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("long clip: status %d body %s", rec.Code, rec.Body.String())
	}

	smallDir := t.TempDir()
	_, small := speechNode(t, "voice_dir: "+smallDir+"\nvoice_max_mb: 1\nstanzas:"+speechStanza("tts-main", ""), fakeSpeech())
	big := activityWAV(1<<20 + 44 + 100)
	if rec := postVoice(t, small, voiceFields(), big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize clip: status %d body %s", rec.Code, rec.Body.String())
	}
	// Over the cap plus the 1 MiB multipart slack, the body cannot be read for
	// the model check; it is still the clip cap's 413, not a parse error.
	huge := activityWAV(2<<20 + 44 + 100)
	if rec := postVoice(t, small, voiceFields(), huge); rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "exceeds the size cap") {
		t.Fatalf("clip over cap and slack: status %d body %s", rec.Code, rec.Body.String())
	}
}

// A fresh router over the same voice_dir lists the voices again.
func TestVoiceLibrarySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	r, h := speechNode(t, "voice_dir: "+dir+"\nstanzas:"+speechStanza("tts-main", ""), fakeSpeech())
	if rec := postVoice(t, h, voiceFields(), activityWAV(1600)); rec.Code != http.StatusCreated {
		t.Fatalf("POST status %d body %s", rec.Code, rec.Body.String())
	}

	reborn := New(r.cfg(), NewLogger(io.Discard, false), "test")
	list := serve(NewHandler(reborn, NewLogger(io.Discard, false)), "GET", "/v1/audio/voices", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"id":"narrator-a"`) {
		t.Fatalf("list after restart: status %d body %s", list.Code, list.Body.String())
	}
}

func captureSpeechBody(got chan<- []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/audio/speech" {
			w.WriteHeader(http.StatusOK)
			return
		}
		b, _ := io.ReadAll(req.Body)
		got <- b
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFF"))
	}
}

// A speech request by library voice reaches the backend as the stanza's
// clone_mode says, with voice removed and the reference attached.
func TestSpeechUsesLibraryVoiceInCloneMode(t *testing.T) {
	tests := []struct {
		name  string
		mode  string
		check func(t *testing.T, body map[string]json.RawMessage, clip []byte)
	}{
		{
			name: "ref_audio",
			mode: "",
			check: func(t *testing.T, body map[string]json.RawMessage, clip []byte) {
				var uri string
				if json.Unmarshal(body["ref_audio"], &uri) != nil || !strings.HasPrefix(uri, "data:audio/wav;base64,") {
					t.Fatalf("ref_audio = %q", uri)
				}
				decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, "data:audio/wav;base64,"))
				if err != nil || !bytes.Equal(decoded, clip) {
					t.Fatalf("ref_audio does not carry the stored clip")
				}
				var text, language string
				json.Unmarshal(body["ref_text"], &text)
				json.Unmarshal(body["language"], &language)
				if text != voiceFields()["ref_text"] || language != "English" {
					t.Fatalf("ref_text = %q language = %q", text, language)
				}
				if _, ok := body["speaker_ref_b64"]; ok {
					t.Fatal("speaker_ref_b64 set in ref_audio mode")
				}
			},
		},
		{
			name: "speaker_ref_b64",
			mode: "\n    clone_mode: speaker_ref_b64",
			check: func(t *testing.T, body map[string]json.RawMessage, clip []byte) {
				var b64, language string
				json.Unmarshal(body["speaker_ref_b64"], &b64)
				json.Unmarshal(body["lang"], &language)
				decoded, err := base64.StdEncoding.DecodeString(b64)
				if err != nil || !bytes.Equal(decoded, clip) || language != "English" {
					t.Fatalf("speaker_ref_b64/lang wrong: %q %q (%v)", b64, language, err)
				}
				if _, ok := body["ref_audio"]; ok {
					t.Fatal("ref_audio set in speaker_ref_b64 mode")
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			got := make(chan []byte, 1)
			_, h := speechNode(t, "voice_dir: "+dir+"\nstanzas:"+speechStanza("tts-main", tc.mode), captureSpeechBody(got))
			clip := activityWAV(1600)
			if rec := postVoice(t, h, voiceFields(), clip); rec.Code != http.StatusCreated {
				t.Fatalf("POST voice status %d body %s", rec.Code, rec.Body.String())
			}
			rec := serve(h, "POST", "/v1/audio/speech", `{"model":"tts-main","input":"hi","voice":"narrator-a"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("speech status %d body %s", rec.Code, rec.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(<-got, &body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body["voice"]; ok {
				t.Fatal("voice still present after injection")
			}
			tc.check(t, body, clip)
		})
	}
}

// A voice name that is not a library id passes through byte-identical: it
// never becomes a file path inside or beside voice_dir.
func TestSpeechVoiceNameCannotEscapeVoiceDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "voices")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A valid record pair one level above voice_dir; a traversal name would use it.
	side, err := json.Marshal(voiceRecord{ID: "x", Name: "x", RefText: "decoy", Language: "English"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "x.json"), side, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "x.wav"), activityWAV(1600), 0o600); err != nil {
		t.Fatal(err)
	}

	got := make(chan []byte, 1)
	_, h := speechNode(t, "voice_dir: "+dir+"\nstanzas:"+speechStanza("tts-main", ""), captureSpeechBody(got))
	want := `{"model":"tts-main","input":"hi","voice":"../x"}`
	if rec := serve(h, "POST", "/v1/audio/speech", want); rec.Code != http.StatusOK {
		t.Fatalf("speech status %d body %s", rec.Code, rec.Body.String())
	}
	if body := <-got; string(body) != want {
		t.Fatalf("traversal name changed the body: %s", body)
	}
}

// With no voice_dir the library opens no file at all: a record named after the
// requested voice in the working directory is left unread.
func TestSpeechVoiceWithoutVoiceDirSkipsFilesystem(t *testing.T) {
	t.Chdir(t.TempDir())
	side, err := json.Marshal(voiceRecord{ID: "alloy", Name: "alloy", RefText: "decoy", Language: "English"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("alloy.json", side, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("alloy.wav", activityWAV(1600), 0o600); err != nil {
		t.Fatal(err)
	}

	got := make(chan []byte, 1)
	_, h := speechNode(t, "stanzas:"+speechStanza("tts-main", ""), captureSpeechBody(got))
	want := `{"model":"tts-main","input":"hi","voice":"alloy"}`
	if rec := serve(h, "POST", "/v1/audio/speech", want); rec.Code != http.StatusOK {
		t.Fatalf("speech status %d body %s", rec.Code, rec.Body.String())
	}
	if body := <-got; string(body) != want {
		t.Fatalf("voice_dir-less request changed the body: %s", body)
	}
}

// A request for a mesh id only a peer serves spills with the library voice
// already resolved, so the peer needs no copy of it.
func TestPeerSpillCarriesResolvedVoice(t *testing.T) {
	dir := t.TempDir()
	got := make(chan []byte, 1)
	peer := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			w.WriteHeader(http.StatusOK)
			return
		}
		b, _ := io.ReadAll(req.Body)
		got <- b
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFF"))
	})
	r, h := speechNode(t, fmt.Sprintf("voice_dir: %s\npeers:\n  - name: alpha\n    kind: router\n    base_url: http://127.0.0.1:%%d\n    models: [shared-tts]\n", dir), peer)
	r.peers.Set("alpha", &Telemetry{Node: "alpha"})

	clip := activityWAV(1600)
	if rec := postVoice(t, h, voiceFields(), clip); rec.Code != http.StatusCreated {
		t.Fatalf("POST voice status %d body %s", rec.Code, rec.Body.String())
	}
	rec := serve(h, "POST", "/v1/audio/speech", `{"model":"shared-tts","input":"hi","voice":"narrator-a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("spilled speech status %d body %s", rec.Code, rec.Body.String())
	}
	raw := <-got
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["voice"]; ok {
		t.Fatal("voice still present at the peer")
	}
	var uri string
	json.Unmarshal(body["ref_audio"], &uri)
	if !strings.HasPrefix(uri, "data:audio/wav;base64,") {
		t.Fatalf("peer got no resolved ref_audio: %s", raw)
	}
}

func TestCloneModeConfig(t *testing.T) {
	cfg, err := config.Parse([]byte("stanzas:\n  - model_id: tts\n    command: fake\n    api_type: speech\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Stanza("tts").CloneMode; got != config.CloneModeRefAudio {
		t.Fatalf("default clone_mode = %q", got)
	}
	if _, err := config.Parse([]byte("stanzas:\n  - model_id: tts\n    command: fake\n    api_type: speech\n    clone_mode: bogus\n")); err == nil {
		t.Fatal("bogus clone_mode parsed")
	}
	if _, err := config.Parse([]byte("stanzas:\n  - model_id: chat\n    command: fake\n    clone_mode: ref_audio\n")); err == nil {
		t.Fatal("clone_mode on a chat stanza parsed")
	}
}

// A POST that names its model in the multipart form reaches the speech
// stanza's backend; the library does not answer 400 for missing library
// fields. Without a model in the query or body the library still creates the
// voice.
func TestVoiceUploadWithBodyModelReachesBackend(t *testing.T) {
	dir := t.TempDir()
	var gotModel, gotFile string
	var gotPost bool
	backend := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/audio/voices" {
			gotPost = true
			if err := req.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			gotModel = req.FormValue("model")
			f, _, err := req.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer f.Close()
			raw, _ := io.ReadAll(f)
			gotFile = string(raw)
			writeJSON(w, map[string]any{"id": "tts-clone"})
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	_, h := speechNode(t, "voice_dir: "+dir+"\nstanzas:"+speechStanza("tts-main", ""), backend)

	fields := voiceFields()
	fields["model"] = "tts-main"
	clip := activityWAV(1600)
	rec := postVoice(t, h, fields, clip)
	if rec.Code != http.StatusOK || gotModel != "tts-main" || gotFile != string(clip) {
		t.Fatalf("backend upload = %d model %q file %q body %s", rec.Code, gotModel, gotFile, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "tts-clone.wav")); !os.IsNotExist(err) {
		t.Fatal("the library stored a backend upload")
	}

	// No model in query or body: the library still creates the voice.
	if rec := postVoice(t, h, voiceFields(), clip); rec.Code != http.StatusCreated {
		t.Fatalf("library upload = %d body %s", rec.Code, rec.Body.String())
	}
	// A JSON body model goes to the stanza too.
	gotModel, gotPost = "", false
	if rec := serve(h, "POST", "/v1/audio/voices", `{"model":"tts-main","name":"Narrator"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("json upload = %d body %s", rec.Code, rec.Body.String())
	}
	if !gotPost {
		t.Fatal("json model part never reached the backend")
	}
}

// A backend-native upload is capped by max_request_body_mb, not the library
// clip cap: a body over voice_max_mb + 1 MiB still reaches the stanza. An
// oversized library upload still answers 413 naming the clip cap.
func TestVoiceUploadWithBodyModelUsesRequestBodyCap(t *testing.T) {
	dir := t.TempDir()
	var gotModel string
	var gotSize int64
	backend := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/audio/voices" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotModel = req.FormValue("model")
		f, _, err := req.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		gotSize, _ = io.Copy(io.Discard, f)
		writeJSON(w, map[string]any{"id": "tts-clone"})
	})
	_, h := speechNode(t, "voice_dir: "+dir+"\nstanzas:"+speechStanza("tts-main", ""), backend)

	clip := activityWAV(12 << 20)
	rec := postVoice(t, h, map[string]string{"model": "tts-main"}, clip)
	if rec.Code != http.StatusOK || gotModel != "tts-main" || gotSize != int64(len(clip)) {
		t.Fatalf("backend upload = %d model %q size %d body %s", rec.Code, gotModel, gotSize, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "narrator-a.wav")); !os.IsNotExist(err) {
		t.Fatal("the library stored a backend upload")
	}

	// The clip cap still applies to a library write, and names the clip.
	rec = postVoice(t, h, voiceFields(), clip)
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "voice clip exceeds the size cap") {
		t.Fatalf("library upload = %d body %s", rec.Code, rec.Body.String())
	}
}

// A model-less voice list unions the library voices with the backend presets;
// ?model= still asks just that stanza.
func TestVoiceListUnionsLibraryAndBackends(t *testing.T) {
	dir := t.TempDir()
	_, h := speechNode(t, "voice_dir: "+dir+"\nstanzas:"+speechStanza("tts-main", ""), fakeSpeech("alto"))

	if rec := postVoice(t, h, voiceFields(), activityWAV(1600)); rec.Code != http.StatusCreated {
		t.Fatalf("library upload = %d body %s", rec.Code, rec.Body.String())
	}
	rec := serve(h, "GET", "/v1/audio/voices", "")
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	byID := map[string]map[string]any{}
	for _, row := range out.Data {
		id, _ := row["id"].(string)
		byID[id] = row
	}
	lib, ok := byID["narrator-a"]
	if !ok || lib["source"] != "library" || lib["language"] != "English" {
		t.Fatalf("library row missing: %v", lib)
	}
	backend, ok := byID["alto"]
	if !ok || backend["model"] != "tts-main" || backend["source"] != nil {
		t.Fatalf("backend preset missing: %v", backend)
	}
	if rec := serve(h, "GET", "/v1/audio/voices?model=tts-main", ""); !strings.Contains(rec.Body.String(), `"data":["alto"`) {
		t.Fatalf("?model=tts-main: %s", rec.Body.String())
	}
}

// When every candidate stanza fails to list its voices, the library rows are
// still answered, not replaced by a 502 that depends on how many stanzas ran.
func TestVoiceListKeepsLibraryWhenEveryBackendFails(t *testing.T) {
	dir := t.TempDir()
	down := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/audio/voices" {
			http.Error(w, "backend down", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	r, h := speechNode(t, "voice_dir: "+dir+"\nstanzas:"+
		speechStanza("tts-main", "\n    match: {path_default: true}")+speechStanza("tts-alt", ""),
		down, down)

	if rec := postVoice(t, h, voiceFields(), activityWAV(1600)); rec.Code != http.StatusCreated {
		t.Fatalf("library upload = %d body %s", rec.Code, rec.Body.String())
	}
	if _, err := r.Load("tts-alt"); err != nil {
		t.Fatal(err)
	}
	if !r.anyRunning(r.cfg().Stanza("tts-alt")) {
		t.Fatal("tts-alt was not running; the union did not take its failure path")
	}
	rec := serve(h, "GET", "/v1/audio/voices", "")
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	if len(out.Data) != 1 || out.Data[0]["id"] != "narrator-a" || out.Data[0]["source"] != "library" {
		t.Fatalf("library rows = %v", out.Data)
	}
}

// An unreadable voice_dir degrades like a failed stanza: the backend's voice
// list still answers instead of the whole request returning 500.
func TestVoiceListServesBackendsWhenLibraryUnreadable(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "voices")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, h := speechNode(t, "voice_dir: "+blocked+"\nstanzas:"+speechStanza("tts-main", ""), fakeSpeech("alto"))

	rec := serve(h, "GET", "/v1/audio/voices", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"alto"`) || !strings.Contains(rec.Body.String(), `"model":"tts-main"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
}
