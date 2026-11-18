package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const fallbackPool = `
pools:
  shared-pool:
    targets: [alpha/shared-model, bravo/shared-model]
    spillover: 1
`

const fallbackPool3 = `
pools:
  shared-pool:
    targets: [alpha/shared-model, bravo/shared-model, charlie/shared-model]
    spillover: 1
`

func TestPoolFallsBackWhenFirstTargetAnswers5xx(t *testing.T) {
	a := newWalkPeer(t, http.StatusOK, "")
	a.chatStatus = http.StatusInternalServerError
	b := newWalkPeer(t, http.StatusOK, "")
	r := walkRouter(t, fallbackPool, a, b)

	rec := chat(r, "shared-pool")
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("status = %d, body = %q, want bravo's 200", rec.Code, rec.Body.String())
	}
	if a.chats.Load() != 1 || b.chats.Load() != 1 {
		t.Fatalf("alpha chats = %d, bravo chats = %d, want 1 and 1", a.chats.Load(), b.chats.Load())
	}
	if !strings.Contains(strings.Join(r.logger.RecentMesh(10), "\n"), "skipped") {
		t.Fatal("the fallback should be logged on the mesh ring")
	}
}

func TestPoolFallsBackWhenFirstTargetDropsTheConnection(t *testing.T) {
	// Accepts the load, then closes the socket on the request itself.
	dropper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			return
		}
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(dropper.Close)
	a := &walkPeer{srv: dropper}
	b := newWalkPeer(t, http.StatusOK, "")
	r := walkRouter(t, fallbackPool, a, b)

	if rec := chat(r, "shared-pool"); rec.Code != http.StatusOK || b.chats.Load() != 1 {
		t.Fatalf("status = %d, bravo chats = %d, want 200 and 1", rec.Code, b.chats.Load())
	}
}

func TestPoolDoesNotRetryOnceTheResponseHasStarted(t *testing.T) {
	a := newWalkPeer(t, http.StatusOK, "")
	b := newWalkPeer(t, http.StatusOK, "")
	r := walkRouter(t, fallbackPool, a, b)

	rec := chat(r, "shared-pool")
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if b.loads.Load() != 0 || b.chats.Load() != 0 {
		t.Fatalf("bravo loads = %d, chats = %d, want 0 once alpha wrote bytes", b.loads.Load(), b.chats.Load())
	}

	// A 4xx is the client's answer, not a target failure.
	a.chatStatus = http.StatusBadRequest
	if rec := chat(r, "shared-pool"); rec.Code != http.StatusBadRequest || b.chats.Load() != 0 {
		t.Fatalf("status = %d, bravo chats = %d, want alpha's 400 and no retry", rec.Code, b.chats.Load())
	}
}

func TestPoolFallbackAfterFailedLoadWalk(t *testing.T) {
	a := newWalkPeer(t, http.StatusServiceUnavailable, "30") // refuses the load
	b := newWalkPeer(t, http.StatusOK, "")
	b.chatStatus = http.StatusBadGateway // accepts the load, then the request fails
	c := newWalkPeer(t, http.StatusOK, "")
	r := walkRouter(t, fallbackPool3, a, b, c)

	if rec := chat(r, "shared-pool"); rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("status = %d, body = %q, want charlie's 200", rec.Code, rec.Body.String())
	}
	if a.chats.Load() != 0 || b.chats.Load() != 1 || c.chats.Load() != 1 {
		t.Fatalf("chats = %d/%d/%d, want 0/1/1", a.chats.Load(), b.chats.Load(), c.chats.Load())
	}
}

func TestPoolAllTargetsFailingAnswers503(t *testing.T) {
	a := newWalkPeer(t, http.StatusOK, "")
	a.chatStatus = http.StatusInternalServerError
	b := newWalkPeer(t, http.StatusOK, "")
	b.chatStatus = http.StatusInternalServerError
	r := walkRouter(t, fallbackPool, a, b)

	rec := chat(r, "shared-pool")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "answered 500") {
		t.Fatalf("status = %d, body = %q, want 503 naming the last failure", rec.Code, rec.Body.String())
	}
}
