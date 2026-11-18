package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

func TestHeadroomRefusesLoadThatMissesFreeMinusHeadroom(t *testing.T) {
	l := NewLedger()
	l.headroomMB = 1000
	l.SetGPUs([]*GPUState{gpu(0, 10000)})
	if l.CanAdmit("a", &config.Stanza{ModelID: "a", VramMB: 9500}) {
		t.Fatal("9500 fits raw free but must miss 10000-1000")
	}
	if !l.CanAdmit("a", &config.Stanza{ModelID: "a", VramMB: 9000}) {
		t.Fatal("9000 fits free minus headroom")
	}
	// The tighter of live and ledger free: live is lower here.
	l.SetGPUs([]*GPUState{{Index: 0, TotalMB: 10000, FreeMB: 4000}})
	if l.CanAdmit("b", &config.Stanza{ModelID: "b", VramMB: 3500}) {
		t.Fatal("live free 4000 less headroom must refuse 3500")
	}
	// Headroom is not a reservation: the booked MB is the stanza budget.
	if _, ok := l.Reserve("c", &config.Stanza{ModelID: "c", VramMB: 3000}); !ok {
		t.Fatal("3000 fits live free minus headroom")
	}
	if got := l.reservationUsed("c")[0]; got != 3000 {
		t.Fatalf("reservation = %d, want the stanza budget 3000", got)
	}
}

func TestHeadroomTakenOffEachCardOfASplit(t *testing.T) {
	l := NewLedger()
	l.headroomMB = 1000
	l.SetGPUs([]*GPUState{gpu(0, 10000), gpu(1, 10000)})
	split := func(mb int64) *config.Stanza { return &config.Stanza{ModelID: "m", VramMB: mb, Device: "0,1"} }
	if l.CanAdmit("m", split(19000)) {
		t.Fatal("19000 is 9500 a card; each card offers 9000 after headroom")
	}
	if !l.CanAdmit("m", split(18000)) {
		t.Fatal("18000 is 9000 a card and fits")
	}
}

func TestHeadroomDefaultsToZero(t *testing.T) {
	l := NewLedger()
	l.SetGPUs([]*GPUState{gpu(0, 10000)})
	if !l.CanAdmit("a", &config.Stanza{ModelID: "a", VramMB: 10000}) {
		t.Fatal("without headroom the last megabyte is bookable")
	}
}

func TestHeadroomConfigRejectsNegative(t *testing.T) {
	_, err := config.Parse([]byte("headroom_mb: -1\n"))
	if err == nil || !strings.Contains(err.Error(), "headroom_mb") {
		t.Fatalf("err = %v, want headroom_mb error", err)
	}
}

func headroomHandler(t *testing.T, headroom string) (http.Handler, *Router) {
	t.Helper()
	port, _ := healthServer(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
%s
start_port: 5900
stanzas:
  - model_id: big
    command: "fake --port {port}"
    vram_mb: 9500
    health_check: /
    spin_up_seconds: 5
    port: %d
  - model_id: small
    command: "fake --port {port}"
    vram_mb: 9000
    health_check: /
    spin_up_seconds: 5
    port: %d
`, headroom, port, port)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() {
		for _, id := range []string{"big", "small"} {
			_ = r.Unload(id)
		}
	})
	return NewHandler(r, logger), r
}

func pullTelemetry(t *testing.T, h http.Handler) Telemetry {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/_router/telemetry", nil))
	if strings.Contains(rec.Body.String(), "cooldown") {
		t.Fatalf("telemetry carries a cooldown field: %s", rec.Body)
	}
	var tel Telemetry
	if err := json.Unmarshal(rec.Body.Bytes(), &tel); err != nil {
		t.Fatalf("telemetry json: %v", err)
	}
	return tel
}

func postLoad(h http.Handler, id string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("POST", "/_router/load", strings.NewReader(`{"model_id":"`+id+`"}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestHTTPHeadroomRefusesLoadAndShrinksTelemetry(t *testing.T) {
	h, r := headroomHandler(t, "headroom_mb: 1000")
	g := pullTelemetry(t, h).GPUs[0]
	if g.FreeMB != 9000 || g.FreeIfStaleEvictedMB != 9000 || g.FreeIfIdleEvictedMB != 9000 {
		t.Fatalf("published free = %d/%d/%d, want 9000 each", g.FreeMB, g.FreeIfStaleEvictedMB, g.FreeIfIdleEvictedMB)
	}
	if code := postLoad(h, "big"); code == http.StatusOK {
		t.Fatal("big fits raw free but must be refused after headroom")
	}
	if code := postLoad(h, "small"); code != http.StatusOK {
		t.Fatalf("small load = %d, want 200", code)
	}
	// The budget is untouched and the reservation is the budget alone.
	if got := r.cfg.Stanza("small").VramMB; got != 9000 {
		t.Fatalf("vram_mb = %d, want 9000", got)
	}
	if got := r.ledger.reservationUsed("small")[0]; got != 9000 {
		t.Fatalf("reservation = %d, want 9000", got)
	}
	// Loaded and idle: idle-evicted free is the whole card less headroom.
	if got := pullTelemetry(t, h).GPUs[0].FreeIfIdleEvictedMB; got != 9000 {
		t.Fatalf("idle-evicted free = %d, want 9000", got)
	}
}

func TestHTTPNoHeadroomLeavesPublishedFreeAndAdmission(t *testing.T) {
	h, _ := headroomHandler(t, "")
	g := pullTelemetry(t, h).GPUs[0]
	if g.FreeMB != 10000 || g.FreeIfStaleEvictedMB != 10000 || g.FreeIfIdleEvictedMB != 10000 {
		t.Fatalf("published free = %d/%d/%d, want 10000 each", g.FreeMB, g.FreeIfStaleEvictedMB, g.FreeIfIdleEvictedMB)
	}
	if code := postLoad(h, "big"); code != http.StatusOK {
		t.Fatalf("big load = %d, want 200 with no headroom", code)
	}
}
