package router

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// activityWAV is an n-byte PCM WAV: mono, 8 kHz, 8 bits, so (n-44)/8000 s long.
func activityWAV(n int) []byte {
	buf := make([]byte, n)
	copy(buf, "RIFF")
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

// maskedFrame is one client websocket frame with a fixed mask key.
func maskedFrame(op byte, payload []byte) []byte {
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	out := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		out = append(out, 0x80|byte(n))
	default:
		out = append(out, 0x80|126, byte(n>>8), byte(n))
	}
	out = append(out, mask[:]...)
	for i, b := range payload {
		out = append(out, b^mask[i%4])
	}
	return out
}

func onlyEntry(t *testing.T, r *Router) ActivityEntry {
	t.Helper()
	entries := r.Activity().List()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1: %+v", len(entries), entries)
	}
	return entries[0]
}

func TestWSFramesCountsAndReadsCloseCode(t *testing.T) {
	stream := append(maskedFrame(1, []byte(`{"type":"ping"}`)), maskedFrame(2, make([]byte, 300))...)
	stream = append(stream, maskedFrame(9, nil)...)
	stream = append(stream, maskedFrame(8, []byte{0x0f, 0xa0, 'b', 'y', 'e'})...)
	var f wsFrames
	code := 0
	// Byte by byte, so every header and payload is split across reads.
	for i := range stream {
		if c := f.feed(stream[i : i+1]); c != 0 {
			code = c
		}
	}
	if f.n != 4 || code != 4000 {
		t.Fatalf("frames = %d, close code = %d, want 4 and 4000", f.n, code)
	}
	// An unmasked server close frame in one write.
	var g wsFrames
	if c := g.feed([]byte{0x88, 0x02, 0x03, 0xe8}); c != 1000 || g.n != 1 {
		t.Fatalf("server close = %d after %d frames", c, g.n)
	}
}

func TestWavSeconds(t *testing.T) {
	wav := activityWAV(8044)
	if s := wavSeconds(wav, int64(len(wav))); s != 1 {
		t.Fatalf("seconds = %v, want 1", s)
	}
	// A streamed WAV leaves the data size unset: the byte count decides.
	binary.LittleEndian.PutUint32(wav[40:], 0xFFFFFFFF)
	if s := wavSeconds(wav[:64], int64(len(wav))); s != 1 {
		t.Fatalf("streamed seconds = %v, want 1", s)
	}
	if s := wavSeconds([]byte("ID3 not a wav at all"), 20); s != 0 {
		t.Fatalf("mp3 seconds = %v, want 0", s)
	}
}

// A speech row carries characters, voice, length and bytes. Under the capture
// cap the audio is kept and served for the player; over it nothing is kept.
func TestActivitySpeechRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     int
		playable bool
		streamed bool
	}{
		{"under cap", 8044, true, false},
		{"over cap", maxCaptureBody + 8044, false, false},
		// Truncated capture with no data size in the header: the length comes
		// from the full byte count, not the captured prefix.
		{"over cap, streamed header", maxCaptureBody + 8044, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wav := activityWAV(tc.size)
			if tc.streamed {
				binary.LittleEndian.PutUint32(wav[40:], 0xFFFFFFFF)
			}
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/audio/speech" {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("Content-Type", "audio/wav")
				_, _ = w.Write(wav)
			}))
			defer backend.Close()
			r, h := uploadRouter(t, backend.URL, "")

			req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"chat-a","input":"Bonjour café","voice":"juniper"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), wav) {
				t.Fatalf("status = %d, %d bytes", rec.Code, rec.Body.Len())
			}
			e := onlyEntry(t, r)
			m := e.Media
			if m == nil || m.Kind != "speech" || m.InputChars != 12 || m.Voice != "juniper" || m.Playable != tc.playable {
				t.Fatalf("media = %+v", m)
			}
			if want := float64(tc.size-44) / 8000; math.Abs(m.Seconds-want) > 0.001 {
				t.Fatalf("seconds = %v, want %v", m.Seconds, want)
			}
			if e.RespBytes != int64(tc.size) {
				t.Fatalf("resp bytes = %d, want %d", e.RespBytes, tc.size)
			}
			audio := httptest.NewRecorder()
			h.ServeHTTP(audio, httptest.NewRequest("GET", fmt.Sprintf("/_router/activity/%d/audio", e.ID), nil))
			if !tc.playable {
				if audio.Code != http.StatusNotFound {
					t.Fatalf("over-cap audio status = %d, want 404", audio.Code)
				}
				return
			}
			if audio.Code != http.StatusOK || audio.Header().Get("Content-Type") != "audio/wav" || !bytes.Equal(audio.Body.Bytes(), wav) {
				t.Fatalf("audio status = %d, type %q, %d bytes", audio.Code, audio.Header().Get("Content-Type"), audio.Body.Len())
			}
		})
	}
}

// A transcription row names the uploaded file and keeps the first 200
// characters of the transcript.
func TestActivityTranscriptionRow(t *testing.T) {
	text := strings.Repeat("The survey crew logged the bore at dusk. ", 8)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"text": text, "language": "english", "duration": 3.5})
	}))
	defer backend.Close()
	r, h := uploadRouter(t, backend.URL, "")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "chat-a")
	fw, _ := mw.CreateFormFile("file", "site-briefing.wav")
	_, _ = fw.Write(activityWAV(1044))
	_ = mw.Close()
	size := body.Len()
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	m := onlyEntry(t, r).Media
	if m == nil || m.Kind != "transcription" || m.FileName != "site-briefing.wav" || m.InputBytes != int64(size) {
		t.Fatalf("media = %+v, want file site-briefing.wav and %d bytes", m, size)
	}
	if m.OutputChars != utf8.RuneCountInString(text) || m.Text != text[:200] || m.Language != "english" || m.Seconds != 3.5 {
		t.Fatalf("transcript fields = %+v", m)
	}
}

// A realtime session through the proxy is one row, written when the socket
// closes, with frame counts each way and the close code.
func TestActivityRealtimeSessionRow(t *testing.T) {
	frames := maskedFrame(1, []byte(`{"type":"session.update"}`))
	frames = append(frames, maskedFrame(2, make([]byte, 300))...)
	frames = append(frames, maskedFrame(8, []byte{0x03, 0xe8})...)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/realtime" {
			w.WriteHeader(http.StatusOK)
			return
		}
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		// Echo the client's frames back, then end the session.
		_, _ = io.CopyN(conn, brw, int64(len(frames)))
	}))
	defer backend.Close()
	r, h := uploadRouter(t, backend.URL, "")
	front := httptest.NewServer(h)
	defer front.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	nonce := base64.StdEncoding.EncodeToString([]byte("activity-session"))
	fmt.Fprintf(conn, "GET /v1/realtime?model=chat-a HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", nonce)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if n := len(r.Activity().List()); n != 0 {
		t.Fatalf("rows while the session is open = %d, want 0", n)
	}
	if _, err := conn.Write(frames); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(br, make([]byte, len(frames))); err != nil {
		t.Fatalf("echo: %v", err)
	}
	_ = conn.Close()

	for deadline := time.Now().Add(5 * time.Second); len(r.Activity().List()) == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	e := onlyEntry(t, r)
	m := e.Media
	if m == nil || m.Kind != "realtime" || m.FramesIn != 3 || m.FramesOut != 3 || m.CloseCode != 1000 {
		t.Fatalf("media = %+v", m)
	}
	if e.Status != http.StatusSwitchingProtocols || e.Path != "/v1/realtime" {
		t.Fatalf("entry = %+v", e)
	}
}

// Frames the backend sends, ending in its own close frame, are counted as out
// and keep the backend's close code: the proxy copies them over the hijacked
// conn, not the ReadWriter that carried the 101 head.
func TestActivityRealtimeServerCloseCounted(t *testing.T) {
	sent := maskedFrame(1, []byte(`{"type":"response.create"}`))
	sent = append(sent, maskedFrame(2, make([]byte, 200))...)
	var reply []byte // unmasked server frames: text, binary, text, close 1001
	for _, f := range []struct {
		op      byte
		payload []byte
	}{
		{1, []byte(`{"type":"response.created"}`)},
		{2, make([]byte, 40)},
		{1, []byte(`{"type":"response.done"}`)},
		{8, []byte{0x03, 0xe9}},
	} {
		reply = append(reply, 0x80|f.op, byte(len(f.payload)))
		reply = append(reply, f.payload...)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/realtime" {
			w.WriteHeader(http.StatusOK)
			return
		}
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		// Take the client's frames, answer, then close from the server side.
		_, _ = io.CopyN(io.Discard, brw, int64(len(sent)))
		_, _ = conn.Write(reply)
	}))
	defer backend.Close()
	r, h := uploadRouter(t, backend.URL, "")
	front := httptest.NewServer(h)
	defer front.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	nonce := base64.StdEncoding.EncodeToString([]byte("activity-closing"))
	fmt.Fprintf(conn, "GET /v1/realtime?model=chat-a HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", nonce)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if _, err := conn.Write(sent); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(br, got); err != nil || !bytes.Equal(got, reply) {
		t.Fatalf("reply = %x, %v; want %x", got, err, reply)
	}
	_ = conn.Close()

	for deadline := time.Now().Add(5 * time.Second); len(r.Activity().List()) == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	m := onlyEntry(t, r).Media
	if m == nil || m.Kind != "realtime" || m.FramesIn != 2 || m.FramesOut != 4 || m.CloseCode != 1001 {
		t.Fatalf("media = %+v, want 2 in, 4 out, close 1001", m)
	}
}

// A video submit is one row with the job id, state and requested length; a
// voice library write and delete are admin rows.
func TestActivityVideoJobAndVoiceAdminRows(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/videos":
			_, _ = w.Write([]byte(`{"id":"video-7","object":"video","status":"queued","seconds":"4"}`))
		case "/v1/audio/voices":
			_, _ = w.Write([]byte(`{"id":"voice-1","name":"Narrator"}`))
		case "/v1/audio/voices/voice-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer backend.Close()
	r, h := uploadRouter(t, backend.URL, "")

	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/v1/videos", `{"model":"chat-a","prompt":"a lighthouse at dusk","seconds":"4"}`},
		{"POST", "/v1/audio/voices", `{"model":"chat-a","name":"Narrator"}`},
		{"DELETE", "/v1/audio/voices/voice-1?model=chat-a", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code >= 400 {
			t.Fatalf("%s %s status = %d body %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	entries := r.Activity().List() // newest first
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3: %+v", len(entries), entries)
	}
	if m := entries[2].Media; m == nil || m.Kind != "video" || m.JobID != "video-7" || m.State != "queued" || m.Seconds != 4 {
		t.Fatalf("video media = %+v", m)
	}
	for _, e := range entries[:2] {
		if e.Media == nil || e.Media.Kind != voiceAdminKind {
			t.Fatalf("%s %s media = %+v, want admin", e.Method, e.Path, e.Media)
		}
	}
}

func TestActivityMediaPathsAreLogged(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		gen, admin   bool
	}{
		{"POST", "/v1/videos", true, false},
		{"POST", "/v1/audio/generations", true, false},
		{"POST", "/v1/audio/speech/batch", true, false},
		{"GET", "/v1/videos/video-7", false, false},
		{"POST", "/v1/audio/voices", false, true},
		{"DELETE", "/v1/audio/voices/voice-1", false, true},
		{"GET", "/v1/audio/voices", false, false},
	} {
		if got := isGenerationRequest(tc.method, tc.path); got != tc.gen {
			t.Errorf("isGenerationRequest(%s %s) = %v, want %v", tc.method, tc.path, got, tc.gen)
		}
		if got := isVoiceAdmin(tc.method, tc.path); got != tc.admin {
			t.Errorf("isVoiceAdmin(%s %s) = %v, want %v", tc.method, tc.path, got, tc.admin)
		}
	}
}
