package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-router/internal/config"
)

const testKey = "test-key-not-real"

func accessHandler(trusted ...string) http.Handler {
	sec := config.Security{APIKeys: []string{testKey}, TrustedProxies: trusted}
	// Parse is the only place prefixes are built; reuse it.
	raw := "stanzas: []\nsecurity:\n  trusted_proxies: [" + strings.Join(trusted, ",") + "]\n"
	if cfg, err := config.Parse([]byte(raw)); err == nil {
		sec.ProxyPrefixes = cfg.Security.ProxyPrefixes
	}
	return NewAccessHandler(sec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func doFrom(h http.Handler, remote, path string, hdr map[string]string) int {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestAccessKeylessRanges(t *testing.T) {
	h := accessHandler()
	for _, remote := range []string{
		"127.0.0.1:1", "[::1]:1",
		"10.1.2.3:1", "172.16.0.9:1", "172.31.255.1:1", "192.168.1.5:1",
		"169.254.10.1:1", "[fe80::1]:1", "[fe80::1%eth0]:1",
		"[fd12:3456::1]:1",
		"100.64.0.1:1", "100.127.255.254:1", "[fd7a:115c:a1e0::5]:1",
		"[::ffff:192.168.1.5]:1",
	} {
		for _, path := range []string{"/_router/status", "/v1/models"} {
			if got := doFrom(h, remote, path, nil); got != http.StatusOK {
				t.Errorf("%s %s = %d, want 200", remote, path, got)
			}
		}
	}
}

func TestAccessPublicNeedsKey(t *testing.T) {
	h := accessHandler()
	paths := []string{"/_router/status", "/_router/load", "/_router/activity/1", "/logs", "/logs/stream", "/ui/", "/ui", "/metrics", "/v1/models", "/v/x", "/sdapi/v1/txt2img", "/upstream/m/x", "/slots", "/"}
	pub := "203.0.113.7:4000"
	for _, p := range paths {
		if got := doFrom(h, pub, p, nil); got != http.StatusUnauthorized {
			t.Errorf("no key %s = %d, want 401", p, got)
		}
		if got := doFrom(h, pub, p, map[string]string{"X-Api-Key": "wrong"}); got != http.StatusUnauthorized {
			t.Errorf("wrong key %s = %d, want 401", p, got)
		}
		for name, hdr := range map[string]map[string]string{
			"bearer": {"Authorization": "Bearer " + testKey},
			"x-key":  {"x-api-key": testKey},
		} {
			if got := doFrom(h, pub, p, hdr); got != http.StatusOK {
				t.Errorf("%s %s = %d, want 200", name, p, got)
			}
		}
	}
	if got := doFrom(h, pub, "/health", nil); got != http.StatusOK {
		t.Errorf("/health = %d, want 200", got)
	}
	// Browser key prompt: Basic with the key as the password.
	req := httptest.NewRequest("GET", "/ui/", nil)
	req.RemoteAddr = pub
	req.SetBasicAuth("operator", testKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("basic auth /ui/ = %d, want 200", rec.Code)
	}
}

func TestAccessNoKeysConfiguredFailsClosed(t *testing.T) {
	h := NewAccessHandler(config.Security{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if got := doFrom(h, "203.0.113.7:1", "/v1/models", map[string]string{"Authorization": "Bearer "}); got != http.StatusUnauthorized {
		t.Errorf("empty bearer = %d, want 401", got)
	}
	if got := doFrom(h, "203.0.113.7:1", "/v1/models", map[string]string{"X-Api-Key": ""}); got != http.StatusUnauthorized {
		t.Errorf("empty key = %d, want 401", got)
	}
	if got := doFrom(h, "100.64.1.1:1", "/v1/models", nil); got != http.StatusOK {
		t.Errorf("tailscale = %d, want 200", got)
	}
}

func TestAccessForwardedHeaders(t *testing.T) {
	spoof := map[string]string{"X-Forwarded-For": "100.64.0.1", "X-Real-IP": "192.168.1.1"}
	// Untrusted peer: headers are ignored.
	if got := doFrom(accessHandler(), "203.0.113.7:1", "/v1/models", spoof); got != http.StatusUnauthorized {
		t.Errorf("spoofed headers from untrusted peer = %d, want 401", got)
	}
	// Peer is trusted only for a different range: still ignored.
	if got := doFrom(accessHandler("198.51.100.0/24"), "203.0.113.7:1", "/v1/models", spoof); got != http.StatusUnauthorized {
		t.Errorf("spoofed headers from other peer = %d, want 401", got)
	}
	// Trusted proxy (CIDR and exact IP): forwarded address decides.
	for _, trusted := range []string{"203.0.113.0/24", "203.0.113.7"} {
		h := accessHandler(trusted)
		if got := doFrom(h, "203.0.113.7:1", "/v1/models", spoof); got != http.StatusOK {
			t.Errorf("trusted %s, private XFF = %d, want 200", trusted, got)
		}
		if got := doFrom(h, "203.0.113.7:1", "/v1/models", map[string]string{"X-Real-IP": "100.64.0.1"}); got != http.StatusOK {
			t.Errorf("trusted %s, private X-Real-IP = %d, want 200", trusted, got)
		}
		if got := doFrom(h, "203.0.113.7:1", "/v1/models", map[string]string{"X-Forwarded-For": "198.51.100.9"}); got != http.StatusUnauthorized {
			t.Errorf("trusted %s, public XFF = %d, want 401", trusted, got)
		}
	}
	// A client-supplied private hop left of the real public client is ignored.
	h := accessHandler("203.0.113.7")
	if got := doFrom(h, "203.0.113.7:1", "/v1/models", map[string]string{"X-Forwarded-For": "10.0.0.1, 198.51.100.9"}); got != http.StatusUnauthorized {
		t.Errorf("prepended private hop = %d, want 401", got)
	}
	if got := doFrom(h, "203.0.113.7:1", "/v1/models", map[string]string{"X-Forwarded-For": "garbage"}); got != http.StatusUnauthorized {
		t.Errorf("garbage XFF = %d, want 401", got)
	}
}

func TestAccessRealRoutes(t *testing.T) {
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Security.APIKeys = []string{testKey}
	logger := NewLogger(&strings.Builder{}, false)
	h := NewAccessHandler(cfg.Security, NewHandler(New(cfg, logger, "router-a"), logger))
	for _, p := range []string{"/_router/status", "/_router/activity", "/logs", "/metrics", "/v1/models"} {
		if got := doFrom(h, "203.0.113.7:1", p, nil); got != http.StatusUnauthorized {
			t.Errorf("%s no key = %d, want 401", p, got)
		}
		if got := doFrom(h, "203.0.113.7:1", p, map[string]string{"Authorization": "Bearer " + testKey}); got != http.StatusOK {
			t.Errorf("%s with key = %d, want 200", p, got)
		}
		if got := doFrom(h, "192.168.1.5:1", p, nil); got != http.StatusOK {
			t.Errorf("%s from LAN = %d, want 200", p, got)
		}
	}
}

func TestAccessRejectionLeaksNoKey(t *testing.T) {
	h := accessHandler()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.RemoteAddr = "203.0.113.7:1"
	req.Header.Set("Authorization", "Bearer secret-presented-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "secret-presented-key") || strings.Contains(rec.Header().Get("WWW-Authenticate"), "secret-presented-key") {
		t.Fatal("presented key echoed in response")
	}
}
