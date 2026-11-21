package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestLocalProxyDropsBrowserOrigin proves the local proxy path drops the
// router's own /ui/ page's site-context headers before they reach the backend,
// while Activity still records them as the client sent them. The request host
// is example.com, so that origin is the router's own.
func TestLocalProxyDropsBrowserOrigin(t *testing.T) {
	var mu sync.Mutex
	var seen http.Header
	backend := func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" { // the rest is the health check
			mu.Lock()
			seen = req.Header.Clone()
			mu.Unlock()
		}
		okChat(w, req)
	}
	rig := newCoolRig(t, backend)
	rec := rig.chat("good",
		"Origin", "http://example.com",
		"Referer", "http://example.com/ui/",
		"Sec-Fetch-Site", "same-origin",
		"X-Keep-Me", "yes",
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	got := seen
	mu.Unlock()
	if got == nil {
		t.Fatal("backend never saw the request")
	}
	for _, h := range []string{"Origin", "Referer", "Sec-Fetch-Site"} {
		if v := got.Get(h); v != "" {
			t.Errorf("backend got %s = %q", h, v)
		}
	}
	if v := got.Get("X-Keep-Me"); v != "yes" {
		t.Errorf("backend lost X-Keep-Me = %q", v)
	}

	capRec := httptest.NewRecorder()
	rig.h.ServeHTTP(capRec, httptest.NewRequest("GET", "/_router/activity/1", nil))
	if capRec.Code != http.StatusOK {
		t.Fatalf("capture status = %d %s", capRec.Code, capRec.Body.String())
	}
	var cap ActivityCapture
	if err := json.Unmarshal(capRec.Body.Bytes(), &cap); err != nil {
		t.Fatalf("capture json: %v", err)
	}
	if v := cap.ReqHeaders["Origin"]; len(v) != 1 || v[0] != "http://example.com" {
		t.Fatalf("captured Origin = %v", v)
	}
}

// TestLocalProxyForwardsForeignOrigin proves the local proxy leaves a foreign
// page's site-context headers in place, so the backend can still refuse the
// cross-site call.
func TestLocalProxyForwardsForeignOrigin(t *testing.T) {
	var mu sync.Mutex
	var seen http.Header
	backend := func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" { // the rest is the health check
			mu.Lock()
			seen = req.Header.Clone()
			mu.Unlock()
		}
		okChat(w, req)
	}
	rig := newCoolRig(t, backend)
	rec := rig.chat("good",
		"Origin", "http://playground.example",
		"Referer", "http://playground.example/ui/",
		"Sec-Fetch-Site", "cross-site",
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	got := seen
	mu.Unlock()
	if got == nil {
		t.Fatal("backend never saw the request")
	}
	for h, want := range map[string]string{
		"Origin":         "http://playground.example",
		"Referer":        "http://playground.example/ui/",
		"Sec-Fetch-Site": "cross-site",
	} {
		if v := got.Get(h); v != want {
			t.Errorf("backend got %s = %q, want %q", h, v, want)
		}
	}
}
