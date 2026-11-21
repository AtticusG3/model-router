// fake-model is a tiny OpenAI/sd.cpp-compatible backend used to smoke-test the
// model router without loading real models or touching GPUs. It speaks just
// enough of the API surface: /health, /v1/models, /v1/chat/completions
// (streaming + non-streaming), /v1/embeddings, /v1/rerank, /sdapi/v1/txt2img,
// /v1/audio/* (speech, transcriptions, translations, voices, generations),
// /v1/realtime (websocket echo), and the /v1/videos job API (plus the same
// jobs through sd-server's /sdcpp/v1 shape).
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// wsGUID is the RFC 6455 magic string used in the handshake accept key.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type fakeModel struct {
	model       string
	delay       time.Duration
	jobSeconds  time.Duration
	bigBytes    int
	healthDelay time.Duration
	start       time.Time

	mu      sync.Mutex
	voices  map[string]string // id -> file name
	voiceID int
	jobs    map[string]*job
}

type job struct {
	ID       string       `json:"id"`
	Object   string       `json:"object"`
	Model    string       `json:"model"`
	Status   string       `json:"status"`
	Progress int          `json:"progress"`
	Created  int64        `json:"created_at"`
	Result   *sdcppResult `json:"-"`
}

// sdcppResult is the sd-server vid_gen result shape the router's sdcpp adapter reads.
type sdcppResult struct {
	Kind     string `json:"kind"`
	MimeType string `json:"mime_type"`
	B64JSON  string `json:"b64_json"`
}

func main() {
	f := newFakeModelFromFlags()
	mux := http.NewServeMux()
	f.routes(mux)

	log.Printf("fake-model %q listening on :%d", f.model, *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), mux))
}

var port = flag.Int("port", 5900, "listen port")

func newFakeModelFromFlags() *fakeModel {
	var (
		model      = flag.String("model", "fake-model", "model id this backend reports")
		delay      = flag.Duration("delay", 0, "sleep before answering audio uploads")
		jobSeconds = flag.Duration("job-seconds", time.Second, "time for video/audio jobs to complete")
		bigBytes   = flag.Int("big-bytes", 0, "size in bytes of speech and video content responses")
		healthD    = flag.Duration("health-delay", 0, "/health returns 503 for this long after start")
	)
	flag.Parse()
	return &fakeModel{
		model: *model, delay: *delay, jobSeconds: *jobSeconds,
		bigBytes: *bigBytes, healthDelay: *healthD, start: time.Now(),
		voices: map[string]string{}, jobs: map[string]*job{},
	}
}

func (f *fakeModel) routes(mux *http.ServeMux) {
	mux.HandleFunc("/health", f.health)
	mux.HandleFunc("/v1/models", f.models)
	mux.HandleFunc("/v1/chat/completions", f.chatCompletions)
	mux.HandleFunc("/v1/embeddings", f.embeddings)
	mux.HandleFunc("/v1/rerank", f.rerank)
	mux.HandleFunc("/sdapi/v1/txt2img", f.txt2img)
	mux.HandleFunc("POST /v1/audio/speech", f.speech)
	mux.HandleFunc("POST /v1/audio/transcriptions", f.audioUpload)
	mux.HandleFunc("POST /v1/audio/translations", f.audioUpload)
	mux.HandleFunc("GET /v1/audio/voices", f.listVoices)
	mux.HandleFunc("POST /v1/audio/voices", f.addVoice)
	mux.HandleFunc("DELETE /v1/audio/voices/{id}", f.deleteVoice)
	mux.HandleFunc("GET /v1/realtime", f.realtime)
	mux.HandleFunc("POST /v1/videos", f.createJob("video"))
	mux.HandleFunc("GET /v1/videos/{id}", f.getJob)
	mux.HandleFunc("GET /v1/videos/{id}/content", f.jobContent("video/mp4"))
	mux.HandleFunc("DELETE /v1/videos/{id}", f.deleteJob)
	mux.HandleFunc("POST /sdcpp/v1/vid_gen", f.sdcppCreate)
	mux.HandleFunc("GET /sdcpp/v1/jobs/{id}", f.sdcppJob)
	mux.HandleFunc("POST /sdcpp/v1/jobs/{id}/cancel", f.sdcppCancel)
	mux.HandleFunc("POST /v1/audio/generations", f.createJob("audio.generation"))
	mux.HandleFunc("GET /v1/audio/generations/{id}", f.getJob)
	mux.HandleFunc("GET /v1/audio/generations/{id}/content", f.jobContent("audio/mpeg"))
	mux.HandleFunc("DELETE /v1/audio/generations/{id}", f.deleteJob)
}

func (f *fakeModel) health(w http.ResponseWriter, r *http.Request) {
	if time.Since(f.start) < f.healthDelay {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "starting")
		return
	}
	fmt.Fprintln(w, "OK")
}

func (f *fakeModel) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"object": "list",
		"data": []map[string]any{{
			"id": f.model, "object": "model", "created": time.Now().Unix(),
		}},
	})
}

func (f *fakeModel) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	stream, _ := body["stream"].(bool)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		for i := range 3 {
			chunk := map[string]any{
				"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": f.model,
				"choices": []map[string]any{{"index": i, "delta": map[string]any{"content": fmt.Sprintf("tok%d ", i)}}},
			}
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(chunk))
			flusher.Flush()
			time.Sleep(200 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	writeJSON(w, map[string]any{
		"id": "chatcmpl-fake", "object": "chat.completion", "model": f.model,
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "hello from " + f.model}}},
	})
}

func (f *fakeModel) embeddings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"object": "list", "model": f.model,
		"data": []map[string]any{{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}}},
	})
}

func (f *fakeModel) rerank(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"results": []map[string]any{{"index": 0, "relevance_score": 0.99}}})
}

func (f *fakeModel) txt2img(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"images": []string{"iVBORw0KGgo="}, "parameters": map[string]any{"model": f.model}})
}

// speech renders a minimal valid WAV, or raw bytes for pcm/mp3/opus formats.
func (f *fakeModel) speech(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		ResponseFormat string `json:"response_format"`
		Stream         bool   `json:"stream"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	payload := f.audioPayload(body.ResponseFormat)
	ct := map[string]string{
		"pcm": "audio/pcm", "mp3": "audio/mpeg", "opus": "audio/opus",
	}[body.ResponseFormat]
	if ct == "" {
		ct = "audio/wav"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Fake-Model", f.model)

	const chunks = 4
	if body.Stream {
		flusher, _ := w.(http.Flusher)
		for i := range chunks {
			lo := i * len(payload) / chunks
			hi := (i + 1) * len(payload) / chunks
			w.Write(payload[lo:hi])
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
		}
		return
	}
	w.Write(payload)
}

// audioPayload builds the response body: a WAV header plus data (or raw filler
// for non-WAV formats), sized to bigBytes when set.
func (f *fakeModel) audioPayload(format string) []byte {
	n := f.bigBytes
	if n == 0 {
		n = 2048
	}
	if format == "pcm" || format == "mp3" || format == "opus" {
		return make([]byte, n)
	}
	if n < 44 {
		n = 44
	}
	buf := make([]byte, n)
	// RIFF/WAVE header: PCM, mono, 8 kHz, 8 bits, data chunk fills the rest.
	binary.LittleEndian.PutUint32(buf[0:], 0x46464952)
	binary.LittleEndian.PutUint32(buf[4:], uint32(n-8))
	copy(buf[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1)
	binary.LittleEndian.PutUint16(buf[22:], 1)
	binary.LittleEndian.PutUint32(buf[24:], 8000)
	binary.LittleEndian.PutUint32(buf[28:], 8000)
	binary.LittleEndian.PutUint16(buf[32:], 1)
	binary.LittleEndian.PutUint16(buf[34:], 8)
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(n-44))
	return buf
}

// audioUpload echoes the multipart upload so a test can prove the file arrived
// intact; used for both transcriptions and translations.
func (f *fakeModel) audioUpload(w http.ResponseWriter, r *http.Request) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file part: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()
	n, err := io.Copy(io.Discard, file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	model := r.FormValue("model")
	format := r.FormValue("response_format")
	switch format {
	case "text", "srt", "vtt":
		// whisper-server's own types: the router must map these to text/plain.
		w.Header().Set("Content-Type", map[string]string{
			"text": "text/html", "srt": "application/x-subrip", "vtt": "text/vtt",
		}[format])
		fmt.Fprintf(w, "transcribed %s (%d bytes) with %s", hdr.Filename, n, model)
	case "verbose_json":
		writeJSON(w, map[string]any{
			"task":  r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:],
			"model": model, "filename": hdr.Filename, "bytes": n,
			"segments": []map[string]any{{"id": 0, "text": "fake segment"}},
		})
	default:
		writeJSON(w, map[string]any{
			"text":  fmt.Sprintf("transcribed %s (%d bytes) with %s", hdr.Filename, n, model),
			"model": model, "filename": hdr.Filename, "bytes": n,
		})
	}
}

func (f *fakeModel) listVoices(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	ids := make([]string, 0, len(f.voices))
	for id := range f.voices {
		ids = append(ids, id)
	}
	f.mu.Unlock()
	writeJSON(w, map[string]any{"object": "list", "data": ids})
}

func (f *fakeModel) addVoice(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if name == "" || r.FormValue("consent") == "" {
		http.Error(w, "name and consent are required", http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file part: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()
	io.Copy(io.Discard, file)

	f.mu.Lock()
	f.voiceID++
	id := fmt.Sprintf("voice-%d", f.voiceID)
	f.voices[id] = hdr.Filename
	f.mu.Unlock()
	writeJSON(w, map[string]any{"id": id, "name": name, "filename": hdr.Filename})
}

func (f *fakeModel) deleteVoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	_, ok := f.voices[id]
	delete(f.voices, id)
	f.mu.Unlock()
	if !ok {
		http.Error(w, "no such voice", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// startJob creates a queued -> in_progress -> completed job that finishes
// after jobSeconds. The returned snapshot is safe to encode.
func (f *fakeModel) startJob(kind, model string) job {
	f.mu.Lock()
	// Random ids, as real backends make: two instances never share one.
	j := &job{
		ID: kind + "-" + rand.Text(), Object: kind,
		Model: model, Status: "queued", Created: time.Now().Unix(),
	}
	f.jobs[j.ID] = j
	snapshot := *j
	f.mu.Unlock()

	go func() {
		half := f.jobSeconds / 2
		if half < 10*time.Millisecond {
			half = 10 * time.Millisecond
		}
		time.Sleep(half)
		f.mu.Lock()
		if j.Status != "cancelled" {
			j.Status, j.Progress = "in_progress", 50
		}
		f.mu.Unlock()
		time.Sleep(f.jobSeconds - half)
		f.mu.Lock()
		if j.Status != "cancelled" {
			j.Status, j.Progress = "completed", 100
			if kind == "video" {
				j.Result = &sdcppResult{
					Kind: "vid_gen", MimeType: "video/webm",
					B64JSON: base64.StdEncoding.EncodeToString(f.videoPayload()),
				}
			}
		}
		f.mu.Unlock()
	}()
	return snapshot
}

// videoPayload is the clip the sdcpp job result carries.
func (f *fakeModel) videoPayload() []byte {
	n := f.bigBytes
	if n == 0 {
		n = 4096
	}
	return make([]byte, n)
}

// createJob returns a handler starting a queued -> in_progress -> completed
// job that finishes after jobSeconds.
func (f *fakeModel) createJob(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, f.startJob(kind, body.Model))
	}
}

// sdcppCreate starts the same video job for sd-server's native vid_gen route.
func (f *fakeModel) sdcppCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	j := f.startJob("video", body.Model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]any{"id": j.ID, "status": "queued", "poll_url": "/sdcpp/v1/jobs/" + j.ID})
}

// sdcppJob renders the job record in sd-server's shape, including the result
// once completed. The adapter maps in_progress back to "in_progress" itself.
func (f *fakeModel) sdcppJob(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	j, ok := f.jobs[r.PathValue("id")]
	var snapshot job
	if ok {
		snapshot = *j
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	rec := map[string]any{
		"id": snapshot.ID, "created": snapshot.Created,
		"progress": map[string]int{"completed": snapshot.Progress, "total": 100},
	}
	switch snapshot.Status {
	case "in_progress":
		rec["status"] = "generating"
	case "cancelled":
		rec["status"] = "cancelled"
	default:
		rec["status"] = snapshot.Status
	}
	if snapshot.Status == "completed" && snapshot.Result != nil {
		rec["result"] = snapshot.Result
	}
	writeJSON(w, rec)
}

// sdcppCancel ends a queued or running job, as sd-server's cancel route does.
// Cancellation is authoritative only for jobs that have not finished: a
// completed job keeps its result, and a cancelled job never moves on, because
// the startJob goroutine checks the status under the lock before every
// transition.
func (f *fakeModel) sdcppCancel(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	j, ok := f.jobs[r.PathValue("id")]
	status := "cancelled"
	if ok {
		if j.Status == "queued" || j.Status == "in_progress" {
			j.Status, j.Result = "cancelled", nil
		}
		status = j.Status
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"id": r.PathValue("id"), "status": status})
}

func (f *fakeModel) getJob(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	j, ok := f.jobs[r.PathValue("id")]
	var snapshot job
	if ok {
		snapshot = *j
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	writeJSON(w, snapshot)
}

// jobContent returns a few KB (or bigBytes) once the job is done.
func (f *fakeModel) jobContent(contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		j, ok := f.jobs[r.PathValue("id")]
		status := ""
		if ok {
			status = j.Status
		}
		f.mu.Unlock()
		if !ok {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		if status != "completed" {
			http.Error(w, "job not completed yet", http.StatusConflict)
			return
		}
		n := f.bigBytes
		if n == 0 {
			n = 4096
		}
		w.Header().Set("Content-Type", contentType)
		w.Write(make([]byte, n))
	}
}

func (f *fakeModel) deleteJob(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	_, ok := f.jobs[r.PathValue("id")]
	delete(f.jobs, r.PathValue("id"))
	f.mu.Unlock()
	if !ok {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// realtime upgrades to a websocket and echoes: text frames come back as
// response.text.delta events, binary frames come back verbatim.
func (f *fakeModel) realtime(w http.ResponseWriter, r *http.Request) {
	conn, br, ok := wsUpgrade(w, r)
	if !ok {
		return
	}
	defer conn.Close()
	model := r.URL.Query().Get("model")
	for {
		op, payload, err := wsReadFrame(br)
		if err != nil {
			return
		}
		switch op {
		case 8: // close
			wsWriteFrame(conn, 8, string(payload))
			return
		case 1:
			var msg struct {
				Type    string         `json:"type"`
				Session map[string]any `json:"session"`
			}
			if json.Unmarshal(payload, &msg) == nil && msg.Type == "session.update" {
				if m, _ := msg.Session["model"].(string); m != "" {
					model = m
				}
			}
			event := map[string]any{
				"type": "response.text.delta", "delta": string(payload), "model": model,
			}
			if err := wsWriteFrame(conn, 1, mustJSON(event)); err != nil {
				return
			}
		case 2:
			if err := wsWriteFrame(conn, 2, string(payload)); err != nil {
				return
			}
		}
	}
}

// wsUpgrade performs the RFC 6455 handshake without gorilla/websocket. It
// returns the reader net/http gave us so bytes read past the handshake (an
// early frame sent before the 101) are still processed.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (io.ReadWriteCloser, *bufio.Reader, bool) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, nil, false
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return nil, nil, false
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil, false
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		nonce := make([]byte, 16)
		rand.Read(nonce)
		key = base64.StdEncoding.EncodeToString(nonce)
	}
	h := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(h[:])
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	brw.Flush()
	return conn, brw.Reader, true
}

// wsReadFrame reads one client frame (always masked) and returns opcode + payload.
func wsReadFrame(br *bufio.Reader) (byte, []byte, error) {
	b0, err := br.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	b1, err := br.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	op := b0 & 0x0f
	n := int64(b1 & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = int64(binary.BigEndian.Uint64(ext[:]))
		if n > 1<<20 {
			return 0, nil, fmt.Errorf("frame too large: %d", n)
		}
	}
	var mask [4]byte
	if _, err := io.ReadFull(br, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return op, payload, nil
}

// wsWriteFrame writes one unmasked server frame.
func wsWriteFrame(conn io.WriteCloser, op byte, payload string) error {
	var hdr [10]byte
	hdr[0] = 0x80 | op
	n := len(payload)
	switch {
	case n < 126:
		hdr[1] = byte(n)
		_, err := conn.Write(append(hdr[:2], payload...))
		return err
	case n < 65536:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
		_, err := conn.Write(append(hdr[:4], payload...))
		return err
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
		_, err := conn.Write(append(hdr[:10], payload...))
		return err
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
