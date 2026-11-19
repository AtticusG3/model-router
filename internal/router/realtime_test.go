package router

import (
	"bufio"
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
)

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
