package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type nopLog struct{}

func (nopLog) Infof(string, ...any)  {}
func (nopLog) Errorf(string, ...any) {}

const validYAML = "stanzas: []\nlisten: 127.0.0.1:19001\n"

func newTestReloader(t *testing.T, content string) (*Reloader, string, *[]*Config, *sync.Mutex) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	initial, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var applied []*Config
	r := NewReloader(path, initial, func(c *Config) error {
		mu.Lock()
		applied = append(applied, c)
		mu.Unlock()
		return nil
	}, nopLog{})
	return r, path, &applied, &mu
}

func TestReloadValidAppliesAndClearsError(t *testing.T) {
	r, path, applied, _ := newTestReloader(t, validYAML)
	os.WriteFile(path, []byte("stanzas: [\n"), 0o644)
	if r.Reload() == nil {
		t.Fatal("bad yaml accepted")
	}
	os.WriteFile(path, []byte("stanzas: []\nlisten: 127.0.0.1:19002\n"), 0o644)
	if err := r.Reload(); err != nil {
		t.Fatal(err)
	}
	if len(*applied) != 1 || (*applied)[0].Listen != "127.0.0.1:19002" || r.Current() != (*applied)[0] {
		t.Fatalf("apply not called with the new config: %v", *applied)
	}
	if msg, at := r.LastError(); msg != "" || !at.IsZero() {
		t.Errorf("error not cleared: %q", msg)
	}
}

func TestReloadApplyFailureKeepsRunning(t *testing.T) {
	r, path, _, _ := newTestReloader(t, validYAML)
	initial := r.Current()
	r.apply = func(*Config) error { return errors.New("restart m1: spawn failed") }
	os.WriteFile(path, []byte("stanzas: []\nlisten: 127.0.0.1:19004\n"), 0o644)
	if r.Reload() == nil {
		t.Fatal("failed apply reported as success")
	}
	if r.Current() != initial {
		t.Error("current moved to a config that was not applied")
	}
	if msg, _ := r.LastError(); !strings.Contains(msg, "spawn failed") {
		t.Errorf("apply error not recorded: %q", msg)
	}
}

func TestReloadRejectsInvalidKeepsRunning(t *testing.T) {
	bad := map[string]string{
		"bad yaml":       "stanzas: [\n",
		"failed catalog": "stanzas: []\npreload: [no-such-model]\n",
		"missing file":   "",
	}
	for name, content := range bad {
		t.Run(name, func(t *testing.T) {
			r, path, applied, _ := newTestReloader(t, validYAML)
			before := r.Current()
			if name == "missing file" {
				os.Remove(path)
			} else {
				os.WriteFile(path, []byte(content), 0o644)
			}
			if r.Reload() == nil {
				t.Fatal("invalid config accepted")
			}
			if len(*applied) != 0 || r.Current() != before {
				t.Error("running config changed on reject")
			}
			if msg, at := r.LastError(); msg == "" || at.IsZero() {
				t.Error("error not recorded")
			}
		})
	}
}

// writeAtomic swaps the file in by rename: os.WriteFile truncates first, and a
// watcher tick between truncate and write sees an empty (valid) config.
func writeAtomic(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestWatchFileChangeAndSIGHUP(t *testing.T) {
	r, path, applied, mu := newTestReloader(t, validYAML)
	hup := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.watch(ctx, 10*time.Millisecond, hup)
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(*applied) }
	wait := func(n int) {
		t.Helper()
		for i := 0; i < 300 && count() < n; i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if count() != n {
			t.Fatalf("applied = %d, want %d", count(), n)
		}
	}

	// A valid write triggers apply.
	writeAtomic(t, path, "stanzas: []\nlisten: 127.0.0.1:19003\n")
	wait(1)

	// An invalid write is rejected and recorded, with no further apply.
	writeAtomic(t, path, "stanzas: [\n")
	for i := 0; i < 300; i++ {
		if msg, _ := r.LastError(); msg != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if msg, _ := r.LastError(); !strings.Contains(msg, "parse config") {
		t.Fatalf("invalid write not recorded: %q", msg)
	}
	if count() != 1 || r.Current().Listen != "127.0.0.1:19003" {
		t.Fatal("running config changed on reject")
	}

	// SIGHUP re-runs validation with the file untouched.
	writeAtomic(t, path, validYAML)
	wait(2)
	hup <- syscall.SIGHUP
	wait(3)
}

func TestSettleSkipsAFileCaughtMidWrite(t *testing.T) {
	a := fileStamp{size: 1}
	b := fileStamp{size: 2}
	c := fileStamp{size: 3}
	cases := []struct {
		name          string
		last, pending fileStamp
		seen          fileStamp
		apply         bool
		next          fileStamp
	}{
		{"first sighting is held", a, fileStamp{}, b, false, b},
		{"same file on the next poll applies", a, b, b, true, fileStamp{}},
		{"a newer write replaces the pending one", a, b, c, false, c},
		{"the running file is not a change", a, b, a, false, fileStamp{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apply, next := settle(tc.last, tc.pending, tc.seen)
			if apply != tc.apply || next != tc.next {
				t.Fatalf("settle = %v %+v, want %v %+v", apply, next, tc.apply, tc.next)
			}
		})
	}
}
