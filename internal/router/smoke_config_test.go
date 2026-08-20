package router

import (
	"testing"

	"model-router/internal/config"
)

func TestMatchPeerWithRealSmokeConfig(t *testing.T) {
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Logf("peers: %d, peerByName has digger: %v", len(cfg.Peers), cfg.Peer("digger") != nil)

	req := newReq("POST", "http://x/v1/chat/completions", "application/json", `{"model":"digger/remote-model","messages":[]}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Peer != "digger" || ref.PeerID != "remote-model" {
		t.Errorf("got %+v", ref)
	}
}
