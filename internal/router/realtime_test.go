package router

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

// wsBackend answers health checks and load calls with 200, upgrades
// /v1/realtime and echoes every byte back until the client goes. Each
// upgrade's query model is sent on got when got is not nil.
func wsBackend(t *testing.T, got chan<- string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/realtime" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if got != nil {
			got <- req.URL.Query().Get("model")
		}
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		_, _ = io.Copy(conn, brw)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func srvPort(srv *httptest.Server) int { return srv.Listener.Addr().(*net.TCPAddr).Port }

// wsNonce is the handshake key every test sends. RFC 6455 sends a random
// base64 nonce; building it from a plain literal keeps secret scanners quiet.
var wsNonce = base64.StdEncoding.EncodeToString([]byte("model-router-test"))

// dialRealtime runs a websocket handshake for path on h and returns the
// connection once the 101 is in.
func dialRealtime(t *testing.T, h http.Handler, path string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", path, wsNonce)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	return conn, br, resp
}

// echoes sends frame and checks the backend's echo comes back byte for byte.
func echoes(t *testing.T, conn net.Conn, br *bufio.Reader, frame []byte) {
	t.Helper()
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(frame))
	if _, err := io.ReadFull(br, got); err != nil || !bytes.Equal(got, frame) {
		t.Fatalf("echo = %x, %v; want %x", got, err, frame)
	}
}

func realtimeRouter(t *testing.T, yaml string) (*Router, http.Handler) {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() {
		for _, s := range cfg.Stanzas {
			_ = r.Unload(s.ModelID)
		}
	})
	return r, NewHandler(r, NewLogger(io.Discard, false))
}

func hasManaged(r *Router, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.managed[id] != nil
}

func backdate(r *Router, id string) {
	r.mu.Lock()
	m := r.managed[id]
	r.mu.Unlock()
	m.setLastUsed(time.Now().Add(-2 * time.Second))
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// An open session holds its model like a request mid-generation: the idle
// sweep and a competing load leave it alone, the gauge counts it, and once it
// closes the sweep unloads the model.
func TestRealtimeSessionHoldsTheModel(t *testing.T) {
	srv := wsBackend(t, nil)
	r, h := realtimeRouter(t, fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: voice-a
    command: "fake --port {port}"
    vram_mb: 8000
    device: "0"
    idle_ttl_seconds: 1
    health_check: /
    spin_up_seconds: 5
    port: %[1]d
  - model_id: image-b
    command: "fake --port {port}"
    vram_mb: 8000
    device: "0"
    health_check: /
    spin_up_seconds: 5
    port: %[1]d
`, srvPort(srv)))
	conn, br, _ := dialRealtime(t, h, "/v1/realtime?model=voice-a")
	echoes(t, conn, br, maskedFrame(1, []byte(`{"type":"session.update"}`)))
	if !strings.Contains(r.Metrics().Render("test"), `model_router_realtime_sessions{model="voice-a"} 1`) {
		t.Fatalf("gauge missing the open session:\n%s", r.Metrics().Render("test"))
	}

	backdate(r, "voice-a")
	r.SweepStale()
	if !hasManaged(r, "voice-a") {
		t.Fatal("the sweep unloaded a model with an open session")
	}
	if _, err := r.Load("image-b"); err == nil {
		t.Fatal("a load that needs the card must not evict the open session's model")
	}
	if !hasManaged(r, "voice-a") {
		t.Fatal("voice-a was evicted while its session was open")
	}

	_ = conn.Close()
	waitFor(t, "the session to release voice-a", func() bool { return r.occupancyOf("voice-a") == 0 })
	if !strings.Contains(r.Metrics().Render("test"), `model_router_realtime_sessions{model="voice-a"} 0`) {
		t.Fatalf("gauge did not fall after close:\n%s", r.Metrics().Render("test"))
	}
	backdate(r, "voice-a")
	r.SweepStale()
	if hasManaged(r, "voice-a") {
		t.Fatal("the sweep must unload voice-a once its session closed")
	}
}

// With no model in the URL, the router answers the handshake, reads
// session.model from the first frame and serves the named model, which gets
// the frame unchanged.
func TestRealtimeModelFromFirstFrame(t *testing.T) {
	gotA, gotB := make(chan string, 1), make(chan string, 1)
	a, b := wsBackend(t, gotA), wsBackend(t, gotB)
	_, h := realtimeRouter(t, fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: voice-a
    api_type: realtime
    match:
      path_default: true
    command: "fake --port {port}"
    vram_mb: 0
    health_check: /
    spin_up_seconds: 5
    port: %d
  - model_id: voice-b
    api_type: realtime
    command: "fake --port {port}"
    vram_mb: 0
    health_check: /
    spin_up_seconds: 5
    port: %d
`, srvPort(a), srvPort(b)))
	conn, br, resp := dialRealtime(t, h, "/v1/realtime")
	sum := sha1.Sum([]byte(wsNonce + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatalf("Sec-WebSocket-Accept = %q", got)
	}
	echoes(t, conn, br, maskedFrame(1, []byte(`{"type":"session.update","session":{"type":"realtime","model":"voice-b"}}`)))
	if m := <-gotB; m != "voice-b" {
		t.Fatalf("voice-b backend saw model %q", m)
	}
	select {
	case m := <-gotA:
		t.Fatalf("the default backend took the session (model %q)", m)
	default:
	}
}

// A session whose first frame names a peer's model upgrades through that peer
// with the model id rewritten to the peer's own, and frames flow both ways.
func TestRealtimeFirstFrameModelSpillsToThePeer(t *testing.T) {
	got := make(chan string, 1)
	r := walkRouter(t, "", &walkPeer{srv: wsBackend(t, got)})
	conn, br, _ := dialRealtime(t, NewHandler(r, NewLogger(io.Discard, false)), "/v1/realtime")
	echoes(t, conn, br, maskedFrame(1, []byte(`{"type":"session.update","session":{"model":"alpha/shared-model"}}`)))
	if m := <-got; m != "shared-model" {
		t.Fatalf("peer saw model %q, want shared-model", m)
	}
	echoes(t, conn, br, maskedFrame(2, make([]byte, 300)))
}

// No model in the URL and no frame in time, with no realtime default to fall
// back on: the 101 is already out, so the refusal is a 1008 close.
func TestRealtimeNoModelAndNoFrameCloses(t *testing.T) {
	old := realtimeFirstFrameWait
	realtimeFirstFrameWait = 50 * time.Millisecond
	t.Cleanup(func() { realtimeFirstFrameWait = old })
	r := walkRouter(t, "", newWalkPeer(t, http.StatusOK, ""))
	_, br, _ := dialRealtime(t, NewHandler(r, NewLogger(io.Discard, false)), "/v1/realtime")
	frame, err := io.ReadAll(br)
	if err != nil || len(frame) < 4 || frame[0] != 0x88 || frame[2] != 0x03 || frame[3] != 0xf0 {
		t.Fatalf("got %x, %v; want a 1008 close frame", frame, err)
	}
	if !strings.Contains(string(frame[4:]), ErrNoModel.Error()) {
		t.Fatalf("close reason = %q", frame[4:])
	}
}

// session_max_minutes closes a session that outlives it with 1001.
func TestRealtimeSessionMaxMinutesCloses(t *testing.T) {
	old := sessionMinute
	sessionMinute = 100 * time.Millisecond
	t.Cleanup(func() { sessionMinute = old })
	r, h := uploadRouter(t, wsBackend(t, nil).URL, "session_max_minutes: 1")
	_, br, _ := dialRealtime(t, h, "/v1/realtime?model=chat-a")
	frame := make([]byte, 4)
	if _, err := io.ReadFull(br, frame); err != nil || !bytes.Equal(frame, []byte{0x88, 0x02, 0x03, 0xe9}) {
		t.Fatalf("got %x, %v; want a 1001 close frame", frame, err)
	}
	waitFor(t, "the session to end", func() bool { return r.occupancyOf("chat-a") == 0 })
}

// A pool id upgrades on the member it resolves to.
func TestRealtimePoolUpgrades(t *testing.T) {
	_, h := uploadRouter(t, wsBackend(t, nil).URL, "pools:\n  voice-pool:\n    targets: [chat-a]")
	conn, br, _ := dialRealtime(t, h, "/v1/realtime?model=voice-pool")
	echoes(t, conn, br, maskedFrame(1, []byte(`{"type":"session.update"}`)))
}

// A websocket upgrade on /v1/realtime reaches the peer that serves the model
// named in the query, with the model id rewritten to the peer's own, and bytes
// flow both ways after the 101.
func TestRealtimeUpgradeIsProxiedToThePeer(t *testing.T) {
	gotModel := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			return
		}
		gotModel <- req.URL.Query().Get("model")
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		_, _ = io.Copy(conn, brw) // echo
	}))
	t.Cleanup(upstream.Close)
	r := walkRouter(t, "", &walkPeer{srv: upstream})
	front := httptest.NewServer(NewHandler(r, NewLogger(io.Discard, false)))
	t.Cleanup(front.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	q := url.Values{"model": {"alpha/shared-model"}}.Encode()
	// RFC 6455 sends a random base64 nonce as Sec-WebSocket-Key. Build it from
	// a plain literal so a secret scanner does not read the fixture as a key.
	nonce := base64.StdEncoding.EncodeToString([]byte("model-router-test"))
	fmt.Fprintf(conn, "GET /v1/realtime?%s HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", q, nonce)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if m := <-gotModel; m != "shared-model" {
		t.Fatalf("peer saw model %q, want shared-model", m)
	}
	fmt.Fprint(conn, "ping\n")
	if line, err := br.ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("echo = %q, %v", line, err)
	}
}

// Without a model the route fails like any other model route, so the tab can
// show the reason.
func TestRealtimeWithoutModelIsRefused(t *testing.T) {
	r := walkRouter(t, "", newWalkPeer(t, http.StatusOK, ""))
	h := NewHandler(r, NewLogger(io.Discard, false))
	req := httptest.NewRequest("GET", "/v1/realtime", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), ErrNoModel.Error()) {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}
