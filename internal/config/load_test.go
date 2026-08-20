package config

import "testing"

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
