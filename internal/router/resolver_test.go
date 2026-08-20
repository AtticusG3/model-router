package router

import (
	"testing"
)

func TestResolveRefPeerQualified(t *testing.T) {
	cfg := testConfig(t)
	ref := resolveRef("digger/remote-model", cfg)
	if ref.Peer != "digger" {
		t.Errorf("peer = %q, want digger", ref.Peer)
	}
	if ref.PeerID != "remote-model" {
		t.Errorf("peerID = %q, want remote-model", ref.PeerID)
	}
}

func TestResolveRefLocal(t *testing.T) {
	cfg := testConfig(t)
	ref := resolveRef("agents-a1", cfg)
	if ref.Local != "agents-a1" {
		t.Errorf("local = %q", ref.Local)
	}
}

func TestResolveRefPool(t *testing.T) {
	cfg := testConfig(t)
	ref := resolveRef("coding-pool", cfg)
	if ref.Pool != "coding-pool" {
		t.Errorf("pool = %q", ref.Pool)
	}
}

func TestMatchPeerQualifiedFull(t *testing.T) {
	cfg := testConfig(t)
	req := newReq("POST", "http://x/v1/chat/completions", "application/json", `{"model":"digger/remote-model","messages":[]}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Peer != "digger" || ref.PeerID != "remote-model" {
		t.Errorf("got %+v", ref)
	}
}
