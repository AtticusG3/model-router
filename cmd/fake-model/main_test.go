package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestFake starts a fake-model with short-lived settings for tests.
func newTestFake(t *testing.T, tune func(*fakeModel)) *httptest.Server {
	t.Helper()
	f := &fakeModel{
		model: "fake-test", start: time.Now(),
		jobSeconds: 30 * time.Millisecond,
		voices:     map[string]string{}, jobs: map[string]*job{},
	}
	if tune != nil {
		tune(f)
	}
	mux := http.NewServeMux()
	f.routes(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// multipartBody builds a multipart form with the given fields and one file.
func multipartBody(t *testing.T, fields map[string]string, filename string, fileBytes []byte) (*strings.Reader, string) {
	t.Helper()
	var buf strings.Builder
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile("file", filename)
	fw.Write(fileBytes)
	w.Close()
	return strings.NewReader(buf.String()), w.FormDataContentType()
}

func TestSpeechWAVAndFormats(t *testing.T) {
	srv := newTestFake(t, func(f *fakeModel) { f.bigBytes = 1200 })
	resp, err := http.Post(srv.URL+"/v1/audio/speech", "application/json",
		strings.NewReader(`{"model":"m","input":"hi","voice":"alloy","response_format":"wav"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("content type = %q, want audio/wav", ct)
	}
	if len(body) != 1200 {
		t.Fatalf("len = %d, want 1200", len(body))
	}
	if string(body[:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		t.Fatalf("not a WAV header: %x", body[:12])
	}

	for _, tc := range []struct{ format, ct string }{{"pcm", "audio/pcm"}, {"mp3", "audio/mpeg"}, {"opus", "audio/opus"}} {
		resp, err := http.Post(srv.URL+"/v1/audio/speech", "application/json",
			strings.NewReader(fmt.Sprintf(`{"response_format":%q}`, tc.format)))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != tc.ct {
			t.Fatalf("%s: content type = %q, want %q", tc.format, ct, tc.ct)
		}
	}
}

func TestSpeechStreamArrivesInMoreThanOneRead(t *testing.T) {
	srv := newTestFake(t, nil)
	resp, err := http.Post(srv.URL+"/v1/audio/speech", "application/json",
		strings.NewReader(`{"stream":true,"response_format":"pcm"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reads := 0
	buf := make([]byte, 4096)
	total := 0
	for {
		n, err := resp.Body.Read(buf)
		total += n
		if n > 0 {
			reads++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if total != 2048 {
		t.Fatalf("total = %d, want 2048", total)
	}
	if reads < 2 {
		t.Fatalf("chunked stream arrived in %d reads, want more than one", reads)
	}
}

func TestTranscriptionsEchoesUpload(t *testing.T) {
	srv := newTestFake(t, nil)
	for _, path := range []string{"/v1/audio/transcriptions", "/v1/audio/translations"} {
		for _, format := range []string{"json", "text", "verbose_json"} {
			body, ct := multipartBody(t, map[string]string{"model": "fake-test", "response_format": format},
				"clip.wav", []byte("hello-audio"))
			resp, err := http.Post(srv.URL+path, ct, body)
			if err != nil {
				t.Fatal(err)
			}
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s: status = %d, body = %q", path, format, resp.StatusCode, respBody)
			}
			if format == "text" {
				if !strings.Contains(string(respBody), "clip.wav") || !strings.Contains(string(respBody), "11 bytes") {
					t.Fatalf("text echo = %q", respBody)
				}
				continue
			}
			var doc map[string]any
			if err := json.Unmarshal(respBody, &doc); err != nil {
				t.Fatalf("%s %s: %v", path, format, err)
			}
			if doc["filename"] != "clip.wav" || doc["bytes"].(float64) != 11 || doc["model"] != "fake-test" {
				t.Fatalf("%s %s: echo = %v", path, format, doc)
			}
		}
	}
}

func TestVoiceStoreRoundTrip(t *testing.T) {
	srv := newTestFake(t, nil)
	body, ct := multipartBody(t, map[string]string{"name": "narrator", "consent": "yes"}, "voice.wav", []byte("voice-bytes"))
	resp, err := http.Post(srv.URL+"/v1/audio/voices", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	var added map[string]any
	json.NewDecoder(resp.Body).Decode(&added)
	resp.Body.Close()
	id, _ := added["id"].(string)
	if id == "" || added["filename"] != "voice.wav" {
		t.Fatalf("add = %v", added)
	}

	resp, _ = http.Get(srv.URL + "/v1/audio/voices")
	var list struct {
		Data []string `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Data) != 1 || list.Data[0] != id {
		t.Fatalf("list = %v, want [%s]", list.Data, id)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/audio/voices/"+id, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVideoJobReachesCompleted(t *testing.T) {
	srv := newTestFake(t, func(f *fakeModel) { f.bigBytes = 5000 })
	resp, err := http.Post(srv.URL+"/v1/videos", "application/json", strings.NewReader(`{"model":"fake-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created job
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Status != "queued" {
		t.Fatalf("initial status = %q", created.Status)
	}

	var final job
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(srv.URL + "/v1/videos/" + created.ID)
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(resp.Body).Decode(&final)
		resp.Body.Close()
		if final.Status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never completed; last = %+v", final)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final.Progress != 100 || final.Model != "fake-test" {
		t.Fatalf("final job = %+v", final)
	}

	resp, _ = http.Get(srv.URL + "/v1/videos/" + created.ID + "/content")
	ctBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("content type = %q", ct)
	}
	if len(ctBody) != 5000 {
		t.Fatalf("content len = %d, want 5000", len(ctBody))
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/videos/"+created.ID, nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	resp, _ = http.Get(srv.URL + "/v1/videos/" + created.ID)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVideoSDCppJobAPI(t *testing.T) {
	srv := newTestFake(t, func(f *fakeModel) { f.bigBytes = 5000 })
	resp, err := http.Post(srv.URL+"/sdcpp/v1/vid_gen", "application/json", strings.NewReader(`{"prompt":"a kite"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || created.ID == "" || created.Status != "queued" {
		t.Fatalf("create = %d %+v, want 202 queued", resp.StatusCode, created)
	}

	deadline := time.Now().Add(30 * time.Second)
	var final struct {
		Status string `json:"status"`
		Result struct {
			MimeType string `json:"mime_type"`
			B64JSON  string `json:"b64_json"`
		} `json:"result"`
	}
	for {
		resp, err := http.Get(srv.URL + "/sdcpp/v1/jobs/" + created.ID)
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(resp.Body).Decode(&final)
		resp.Body.Close()
		if final.Status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never completed; last = %+v", final)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final.Result.MimeType != "video/webm" || final.Result.B64JSON == "" {
		t.Fatalf("result = %+v, want a WebM clip", final.Result)
	}

	resp, _ = http.Post(srv.URL+"/sdcpp/v1/vid_gen", "application/json", strings.NewReader(`{}`))
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	resp, _ = http.Post(srv.URL+"/sdcpp/v1/jobs/"+created.ID+"/cancel", "application/json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestSDCppCancelIsAuthoritative replays the reported sequence: a job is
// cancelled, then polled after its normal finish time, and must still read
// cancelled with no result. Cancelling a completed job must keep its result.
func TestSDCppCancelIsAuthoritative(t *testing.T) {
	srv := newTestFake(t, func(f *fakeModel) { f.bigBytes = 200 })
	create := func() string {
		resp, err := http.Post(srv.URL+"/sdcpp/v1/vid_gen", "application/json", strings.NewReader(`{"prompt":"a kite"}`))
		if err != nil {
			t.Fatal(err)
		}
		var created struct {
			ID string `json:"id"`
		}
		json.NewDecoder(resp.Body).Decode(&created)
		resp.Body.Close()
		if created.ID == "" {
			t.Fatal("create returned no id")
		}
		return created.ID
	}
	poll := func(id string) (string, bool) {
		resp, err := http.Get(srv.URL + "/sdcpp/v1/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var rec struct {
			Status string         `json:"status"`
			Result map[string]any `json:"result"`
		}
		json.NewDecoder(resp.Body).Decode(&rec)
		resp.Body.Close()
		return rec.Status, rec.Result != nil
	}

	// Cancelled while running: the goroutine must not overwrite the state.
	id := create()
	resp, err := http.Post(srv.URL+"/sdcpp/v1/jobs/"+id+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Well past jobSeconds (30ms) so both goroutine transitions would have fired.
	time.Sleep(150 * time.Millisecond)
	status, hasResult := poll(id)
	if status != "cancelled" || hasResult {
		t.Fatalf("after cancel and finish time: status=%q result=%v, want cancelled with no result", status, hasResult)
	}

	// Completed first, then cancelled: the result must survive.
	id = create()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, _ = poll(id)
		if status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never completed; last status %q", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err = http.Post(srv.URL+"/sdcpp/v1/jobs/"+id+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	status, hasResult = poll(id)
	if status != "completed" || !hasResult {
		t.Fatalf("cancel of a completed job: status=%q result=%v, want completed with its result", status, hasResult)
	}
}

func TestAudioGenerationsUseSameJobShape(t *testing.T) {
	srv := newTestFake(t, nil)
	resp, err := http.Post(srv.URL+"/v1/audio/generations", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var created job
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(srv.URL + "/v1/audio/generations/" + created.ID)
		if err != nil {
			t.Fatal(err)
		}
		var j job
		json.NewDecoder(resp.Body).Decode(&j)
		resp.Body.Close()
		if j.Status == "completed" {
			if j.Object != "audio.generation" {
				t.Fatalf("object = %q", j.Object)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never completed; last = %+v", j)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHealthDelay(t *testing.T) {
	srv := newTestFake(t, func(f *fakeModel) { f.healthDelay = 60 * time.Millisecond })
	resp, _ := http.Get(srv.URL + "/health")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status during cold start = %d, want 503", resp.StatusCode)
	}
	time.Sleep(70 * time.Millisecond)
	resp, _ = http.Get(srv.URL + "/health")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status after warmup = %d, want 200", resp.StatusCode)
	}
}

// wsDial performs the RFC 6455 handshake and returns a client connection.
func wsDial(t *testing.T, url, query string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	nonce := base64.StdEncoding.EncodeToString([]byte("fake-model-test"))
	fmt.Fprintf(conn, "GET /v1/realtime%s HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", query, nonce)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	want := base64.StdEncoding.EncodeToString(func() []byte {
		h := sha1.Sum([]byte(nonce + wsGUID))
		return h[:]
	}())
	if resp.Header.Get("Sec-WebSocket-Accept") != want {
		t.Fatalf("bad accept key %q", resp.Header.Get("Sec-WebSocket-Accept"))
	}
	return conn
}

func wsSendText(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	var hdr [2]byte
	hdr[0] = 0x81 // FIN + text
	hdr[1] = byte(len(payload))
	mask := []byte{0x11, 0x22, 0x33, 0x44}
	masked := []byte(payload)
	for i := range masked {
		masked[i] ^= mask[i%4]
	}
	conn.Write(hdr[:])
	conn.Write(mask)
	conn.Write(masked)
}

func wsRecvFrame(t *testing.T, conn net.Conn) (byte, []byte) {
	t.Helper()
	conn2 := conn
	var hdr [2]byte
	if _, err := io.ReadFull(conn2, hdr[:]); err != nil {
		t.Fatal(err)
	}
	n := int64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(conn2, ext[:]); err != nil {
			t.Fatal(err)
		}
		n = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(conn2, ext[:]); err != nil {
			t.Fatal(err)
		}
		n = int64(binary.BigEndian.Uint64(ext[:]))
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(conn2, payload); err != nil {
		t.Fatal(err)
	}
	return hdr[0] & 0x0f, payload
}

func TestRealtimeEchoesTextAndBinary(t *testing.T) {
	srv := newTestFake(t, nil)
	conn := wsDial(t, srv.URL, "?model=fake-test")
	wsSendText(t, conn, "hello")
	op, payload := wsRecvFrame(t, conn)
	if op != 1 {
		t.Fatalf("text echo opcode = %d", op)
	}
	var event struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "response.text.delta" || event.Delta != "hello" || event.Model != "fake-test" {
		t.Fatalf("event = %+v", event)
	}

	// Binary frames echo verbatim.
	conn.Write([]byte{0x82, 0x03, 0x11, 0x22, 0x33, 0x44, 0x10, 0x20, 0x30}) // masked binary \x01\x02\x03
	op, payload = wsRecvFrame(t, conn)
	if op != 2 || string(payload) != "\x01\x02\x03" {
		t.Fatalf("binary echo op = %d payload = %x", op, payload)
	}
}

func TestRealtimeReadsModelFromSessionUpdate(t *testing.T) {
	srv := newTestFake(t, nil)
	conn := wsDial(t, srv.URL, "")
	session := map[string]any{
		"type":    "session.update",
		"session": map[string]any{"model": "session-model"},
	}
	b, _ := json.Marshal(session)
	wsSendText(t, conn, string(b))
	wsRecvFrame(t, conn) // echo of the session.update itself
	wsSendText(t, conn, "ping")
	_, payload := wsRecvFrame(t, conn)
	var event struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.Model != "session-model" {
		t.Fatalf("model not picked up from session.update: %s", payload)
	}
}
