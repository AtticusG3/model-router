package router

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"model-router/internal/config"
)

// applyBackend is a stand-in model server. When gated, a chat request reaches
// the backend and then blocks until gate is closed, so a test can hold a
// request in flight across a reload.
type applyBackend struct {
	port    int
	gate    chan struct{}
	started chan struct{}
}

func newApplyBackend(t *testing.T, name string, gated bool) *applyBackend {
	t.Helper()
	b := &applyBackend{started: make(chan struct{}, 8)}
	if gated {
		b.gate = make(chan struct{})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusOK) // health
			return
		}
		b.started <- struct{}{}
		if b.gate != nil {
			<-b.gate
		}
		_, _ = w.Write([]byte("ok-" + name))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	b.port, _ = strconv.Atoi(u.Port())
	return b
}

// commandLog records every command the router starts and can refuse some.
type commandLog struct {
	mu   sync.Mutex
	cmds []string
}

func (s *commandLog) spawn(cmd string, _ []string) (Proc, error) {
	s.mu.Lock()
	s.cmds = append(s.cmds, cmd)
	s.mu.Unlock()
	if strings.Contains(cmd, "bad") {
		return nil, errors.New("spawn failed")
	}
	return newFakeProc(), nil
}

func (s *commandLog) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.cmds {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

type applySpec struct {
	tag    string // part of a's command, so a changed tag is a launch change
	ttlA   int
	withB  bool
	aExtra string // extra YAML inside stanza a
	tail   string // top-level YAML after the stanzas
}

func (a *applyBackend) yaml(b *applyBackend, s applySpec) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "stanzas:\n  - model_id: a\n    command: \"fake-a --port {port} --tag %s\"\n"+
		"    health_check: /\n    spin_up_seconds: 5\n    idle_ttl_seconds: %d\n    port: %d\n%s",
		s.tag, s.ttlA, a.port, s.aExtra)
	if s.withB {
		fmt.Fprintf(&sb, "  - model_id: b\n    command: \"fake-b --port {port}\"\n"+
			"    health_check: /\n    spin_up_seconds: 5\n    port: %d\n", b.port)
	}
	sb.WriteString(s.tail)
	return sb.String()
}

func parseApply(t *testing.T, y string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatalf("config: %v\n%s", err, y)
	}
	return cfg
}

func newApplyRouter(t *testing.T, cfg *config.Config) (*Router, *commandLog) {
	t.Helper()
	r := New(cfg, NewLogger(io.Discard, false), "test")
	sl := &commandLog{}
	r.spawn = sl.spawn
	t.Cleanup(func() {
		for _, id := range r.Config().StanzaIDs() {
			_ = r.Unload(id)
		}
	})
	return r, sl
}

func (r *Router) managedFor(key string) *Managed {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.managed[key]
}

func within(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// chatAsync sends a chat request for model and closes done when it is answered.
func chatAsync(r *Router, model string) (rec *httptest.ResponseRecorder, done chan struct{}) {
	done = make(chan struct{})
	rec = httptest.NewRecorder()
	go func() {
		defer close(done)
		got := chat(r, model)
		rec.Code, rec.Body = got.Code, got.Body
	}()
	return rec, done
}

func TestApplyKeepsRunningModelWhenOnlyRoutingChanges(t *testing.T) {
	ba, bb := newApplyBackend(t, "a", false), newApplyBackend(t, "b", false)
	first := applySpec{tag: "v1", ttlA: 0, withB: true, tail: "pools:\n  p:\n    targets: [a]\n"}
	r, sl := newApplyRouter(t, parseApply(t, ba.yaml(bb, first)))
	if _, err := r.Load("a"); err != nil {
		t.Fatal(err)
	}
	before := r.managedFor("a")

	second := applySpec{
		tag: "v1", ttlA: 99, withB: true,
		aExtra: "    filters:\n      stripParams: [top_k]\n",
		tail: "pools:\n  p:\n    targets: [b]\n" +
			"peers:\n  - name: peer1\n    kind: router\n    base_url: http://192.0.2.10:8082\n    models: [far-model]\n",
	}
	if err := r.Apply(parseApply(t, ba.yaml(bb, second))); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if n := sl.count("fake-a"); n != 1 {
		t.Fatalf("a started %d times, want 1 (no restart)", n)
	}
	if after := r.managedFor("a"); after != before || after.State() != StateRunning {
		t.Fatal("a's supervisor was replaced or stopped")
	}
	if got := before.stz().IdleTTLSeconds; got != 99 {
		t.Errorf("running a follows ttl %d, want the new 99", got)
	}
	cfg := r.Config()
	if got := cfg.Pool("p").Targets; len(got) != 1 || got[0] != "b" {
		t.Errorf("pool p targets = %v, want [b]", got)
	}
	if cfg.Peer("peer1") == nil || len(cfg.Stanza("a").Filters.StripParams) != 1 {
		t.Error("peers and filters were not applied")
	}
}

func TestApplyRestartsOnlyTheModelWhoseCommandChanged(t *testing.T) {
	ba, bb := newApplyBackend(t, "a", false), newApplyBackend(t, "b", true)
	first := applySpec{tag: "v1", withB: true}
	r, sl := newApplyRouter(t, parseApply(t, ba.yaml(bb, first)))
	if _, err := r.Load("a"); err != nil {
		t.Fatal(err)
	}
	// A request to b is mid-generation while a is restarted.
	rec, done := chatAsync(r, "b")
	within(t, "b to receive the request", bb.started)
	bBefore := r.managedFor("b")

	second := applySpec{tag: "v2", withB: true}
	if err := r.Apply(parseApply(t, ba.yaml(bb, second))); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if sl.count("tag v1") != 1 || sl.count("tag v2") != 1 {
		t.Fatalf("a starts: v1=%d v2=%d, want 1 and 1", sl.count("tag v1"), sl.count("tag v2"))
	}
	if sl.count("fake-b") != 1 || r.managedFor("b") != bBefore {
		t.Fatal("b was restarted by a change to a")
	}
	select {
	case <-done:
		t.Fatal("b's request finished before the backend released it")
	default:
	}
	close(bb.gate)
	within(t, "b's request", done)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok-b" {
		t.Fatalf("in-flight request to b: %d %q, want 200 ok-b", rec.Code, rec.Body.String())
	}
}

func TestApplyWaitsForInFlightBeforeRestarting(t *testing.T) {
	ba, bb := newApplyBackend(t, "a", true), newApplyBackend(t, "b", false)
	first := applySpec{tag: "v1", withB: true}
	r, sl := newApplyRouter(t, parseApply(t, ba.yaml(bb, first)))
	rec, reqDone := chatAsync(r, "a")
	within(t, "a to receive the request", ba.started)

	next := parseApply(t, ba.yaml(bb, applySpec{tag: "v2", withB: true}))
	applied := make(chan struct{})
	var applyErr error
	go func() {
		defer close(applied)
		applyErr = r.Apply(next)
	}()
	select {
	case <-applied:
		t.Fatal("Apply restarted a while its request was still in flight")
	case <-time.After(300 * time.Millisecond):
	}
	if sl.count("tag v2") != 0 || r.managedFor("a").State() != StateRunning {
		t.Fatal("a was stopped under an in-flight request")
	}

	close(ba.gate)
	within(t, "the request", reqDone)
	within(t, "Apply", applied)
	if applyErr != nil {
		t.Fatalf("Apply: %v", applyErr)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "ok-a" {
		t.Fatalf("in-flight request: %d %q, want 200 ok-a", rec.Code, rec.Body.String())
	}
	if sl.count("tag v2") != 1 {
		t.Fatalf("a not restarted on the new command: %v", sl.cmds)
	}
}

func TestApplyUnloadsRemovedModelAfterItsRequestsFinish(t *testing.T) {
	ba, bb := newApplyBackend(t, "a", false), newApplyBackend(t, "b", true)
	r, sl := newApplyRouter(t, parseApply(t, ba.yaml(bb, applySpec{tag: "v1", withB: true})))
	if _, err := r.Load("a"); err != nil {
		t.Fatal(err)
	}
	rec, reqDone := chatAsync(r, "b")
	within(t, "b to receive the request", bb.started)

	next := parseApply(t, ba.yaml(bb, applySpec{tag: "v1"}))
	applied := make(chan struct{})
	var applyErr error
	go func() {
		defer close(applied)
		applyErr = r.Apply(next)
	}()
	select {
	case <-applied:
		t.Fatal("Apply unloaded b while its request was in flight")
	case <-time.After(300 * time.Millisecond):
	}
	if r.Config().Stanza("b") != nil {
		t.Error("b is still in the catalog after the reload")
	}
	if m := r.managedFor("b"); m == nil || m.State() != StateRunning {
		t.Fatal("b was unloaded before its request finished")
	}

	close(bb.gate)
	within(t, "the request", reqDone)
	within(t, "Apply", applied)
	if applyErr != nil {
		t.Fatalf("Apply: %v", applyErr)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "ok-b" {
		t.Fatalf("in-flight request to b: %d %q, want 200 ok-b", rec.Code, rec.Body.String())
	}
	if r.managedFor("b") != nil {
		t.Error("b still loaded after its requests finished")
	}
	if sl.count("fake-a") != 1 {
		t.Error("a was restarted by removing b")
	}
}

func TestApplyRollsBackWhenARestartFails(t *testing.T) {
	ba, bb := newApplyBackend(t, "a", false), newApplyBackend(t, "b", false)
	first := applySpec{tag: "v1", withB: true, tail: "pools:\n  p:\n    targets: [a]\n"}
	r, sl := newApplyRouter(t, parseApply(t, ba.yaml(bb, first)))
	if _, err := r.Load("a"); err != nil {
		t.Fatal(err)
	}
	old := r.Config()

	next := applySpec{tag: "bad", withB: true, tail: "pools:\n  p:\n    targets: [b]\n"}
	err := r.Apply(parseApply(t, ba.yaml(bb, next)))
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("Apply error = %v, want a rollback error", err)
	}
	if r.Config() != old {
		t.Fatal("catalog changed although the apply failed")
	}
	m := r.managedFor("a")
	if m == nil || m.State() != StateRunning || !strings.Contains(m.stz().Command, "tag v1") {
		t.Fatal("a is not running its previous command after the rollback")
	}
	if sl.count("tag bad") != 1 || sl.count("tag v1") != 2 {
		t.Fatalf("spawns: %v, want one failed bad start and v1 started twice", sl.cmds)
	}
}
