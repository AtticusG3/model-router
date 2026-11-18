package config

import (
	"strings"
	"testing"
)

func TestLoadSmokeConfigA(t *testing.T) {
	cfg, err := Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Peers) != 1 {
		t.Fatalf("peers = %d, want 1", len(cfg.Peers))
	}
	p := cfg.Peer("digger")
	if p == nil {
		t.Fatal("Peer(\"digger\") is nil")
	}
	if p.BaseURL != "http://127.0.0.1:18100" {
		t.Errorf("base_url = %q", p.BaseURL)
	}
	if cfg.Stanza("agents-a1") == nil || cfg.Pool("coding-pool") == nil {
		t.Error("stanza/pool lookup failed")
	}
}

func TestSecurityParse(t *testing.T) {
	t.Setenv(APIKeysEnv, " env-key-one, ,env-key-two ")
	cfg, err := Parse([]byte("stanzas: []\nsecurity:\n  api_keys: [yaml-key]\n  trusted_proxies: [\"10.0.0.0/8\", \"192.0.2.1\", \"::1\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Security.APIKeys) != 3 || cfg.Security.APIKeys[2] != "env-key-two" {
		t.Errorf("keys = %d, want 3 (yaml + env)", len(cfg.Security.APIKeys))
	}
	if len(cfg.Security.ProxyPrefixes) != 3 {
		t.Errorf("prefixes = %v", cfg.Security.ProxyPrefixes)
	}
	for _, bad := range []string{"security:\n  trusted_proxies: [nope]\n", "security:\n  api_keys: [\"\"]\n"} {
		if _, err := Parse([]byte("stanzas: []\n" + bad)); err == nil {
			t.Errorf("Parse(%q) = nil error", bad)
		} else if strings.Contains(err.Error(), "env-key") {
			t.Errorf("error leaks a key: %v", err)
		}
	}
}
