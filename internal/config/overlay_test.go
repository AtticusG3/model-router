package config

import (
	"os"
	"path/filepath"
	"testing"
)

const overlayBase = `
listen: 0.0.0.0:18080
security:
  api_keys: ["k1"]
  trusted_proxies: ["10.99.0.0/8"]
discovery:
  subnets: ["10.99.0.0/24"]
peers:
  - name: digger
    base_url: "http://10.99.0.36:18080"
    models: [sd3.5-inpaint]
  - name: nugget
    base_url: "http://10.99.0.62:8081"
    models: [qwen3-0.6b]
stanzas:
  - model_id: m1
    port: 5900
    command: /bin/true
`

func writeTemp(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestOverlayPathConvention(t *testing.T) {
	got := OverlayPath("/opt/ai/config/model-router.yaml", "buster")
	want := filepath.Join("/opt/ai/config", "local", "buster.yaml")
	if got != want {
		t.Fatalf("OverlayPath = %q, want %q", got, want)
	}
	// No node name: the config's own base name stands in, so a checkout still
	// gets a predictable path.
	got = OverlayPath("/opt/ai/config/model-router.yaml", "")
	want = filepath.Join("/opt/ai/config", "local", "model-router.yaml")
	if got != want {
		t.Fatalf("OverlayPath(no node) = %q, want %q", got, want)
	}
}

func TestOverlayMissingFallsBackToBase(t *testing.T) {
	dir := t.TempDir()
	base := writeTemp(t, dir, "base.yaml", overlayBase)
	cfg, err := LoadWithOverlay(base, filepath.Join(dir, "local", "buster.yaml"))
	if err != nil {
		t.Fatalf("LoadWithOverlay: %v", err)
	}
	if len(cfg.Peers) != 2 || cfg.Peers[0].BaseURL != "http://10.99.0.36:18080" {
		t.Fatalf("missing overlay must leave the base values: %+v", cfg.Peers)
	}
}

func TestOverlayReplacesPeerListAndMergesMaps(t *testing.T) {
	dir := t.TempDir()
	base := writeTemp(t, dir, "base.yaml", overlayBase)
	ov := writeTemp(t, dir, "ov.yaml", `
security:
  trusted_proxies: ["203.0.113.0/24"]
discovery:
  subnets: ["203.0.113.0/24"]
peers:
  - name: digger
    base_url: "http://203.0.113.36:18080"
    models: [sd3.5-inpaint]
`)
	cfg, err := LoadWithOverlay(base, ov)
	if err != nil {
		t.Fatalf("LoadWithOverlay: %v", err)
	}
	// Lists replace: the peer list is the overlay's, not a concatenation.
	if len(cfg.Peers) != 1 || cfg.Peers[0].BaseURL != "http://203.0.113.36:18080" {
		t.Fatalf("peer list not replaced by overlay: %+v", cfg.Peers)
	}
	// Maps merge: the untouched key survives from the base.
	if len(cfg.Security.APIKeys) != 1 || cfg.Security.APIKeys[0] != "k1" {
		t.Fatalf("unrelated map key lost: %+v", cfg.Security)
	}
	if got := cfg.Security.TrustedProxies; len(got) != 1 || got[0] != "203.0.113.0/24" {
		t.Fatalf("trusted_proxies not overridden: %+v", got)
	}
	if len(cfg.Stanzas) != 1 || cfg.Stanzas[0].ModelID != "m1" {
		t.Fatalf("stanzas must survive an overlay that does not mention them: %+v", cfg.Stanzas)
	}
}

func TestOverlayInvalidIsAnError(t *testing.T) {
	dir := t.TempDir()
	base := writeTemp(t, dir, "base.yaml", overlayBase)
	ov := writeTemp(t, dir, "bad.yaml", "peers: [ {name: \"x\"} ]\ndiscovery: {subnets: [\"not-an-ip\"]}\n")
	if _, err := LoadWithOverlay(base, ov); err == nil {
		t.Fatal("an overlay that does not validate must fail, not fall back silently")
	}
	ov2 := writeTemp(t, dir, "bad2.yaml", "security: [oops\n")
	if _, err := LoadWithOverlay(base, ov2); err == nil {
		t.Fatal("unparseable overlay must fail")
	}
}
