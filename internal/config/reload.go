package config

import (
	"context"
	"crypto/sha256"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Logger is the subset of the router logger the reloader needs.
type Logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// Reloader is the reload gate: it loads and fully validates the config file,
// and only a valid result reaches the apply hook. An invalid file, or an
// apply that fails, leaves the running config alone and is recorded for the UI.
type Reloader struct {
	path  string
	apply func(*Config) error
	log   Logger

	baseline fileStamp // the file as loaded; the watch starts here, not whenever the goroutine first runs

	run       sync.Mutex // the watcher, SIGHUP and POST /_router/reload apply one at a time
	mu        sync.Mutex
	current   *Config
	lastErr   string
	lastErrAt time.Time
	lastAt    time.Time // when the last reload finished, ok or not
}

// NewReloader returns a reloader for path. current is the running config;
// apply receives each newly validated config; an error means it was not taken.
func NewReloader(path string, current *Config, apply func(*Config) error, log Logger) *Reloader {
	r := &Reloader{path: path, current: current, apply: apply, log: log}
	r.baseline, _ = r.statOK()
	return r
}

// Reload validates the file and hands a valid config to the apply hook. The
// file, signal and HTTP triggers all come through here and run one at a time,
// so a slow apply delays the next reload.
func (r *Reloader) Reload() error {
	r.run.Lock()
	defer r.run.Unlock()
	cfg, err := Load(r.path)
	if err == nil {
		err = r.apply(cfg)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastAt = time.Now()
	if err != nil {
		r.lastErr, r.lastErrAt = err.Error(), time.Now()
		// Parse and apply errors name a line, field or model, never a value, so this is safe to log.
		r.log.Errorf("config reload rejected, keeping running config: %v", err)
		return err
	}
	r.lastErr, r.lastErrAt = "", time.Time{}
	r.current = cfg
	r.log.Infof("config reload applied: %s", r.path)
	return nil
}

// Current is the last config that was applied (the initial one until then).
func (r *Reloader) Current() *Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// LastError is the most recent rejection and when it happened; empty until
// a reload fails, and cleared again by the next valid reload.
func (r *Reloader) LastError() (string, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr, r.lastErrAt
}

// LastReload is when the most recent reload finished (zero before the first)
// and its error text, empty if it was applied.
func (r *Reloader) LastReload() (time.Time, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastAt, r.lastErr
}

// Watch reloads on SIGHUP and whenever the file's content changes, until ctx ends.
// shortcut: polls the file instead of using fsnotify (no new dependency, survives editor rename-saves), upgrade if sub-second latency matters.
func (r *Reloader) Watch(ctx context.Context, every time.Duration) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	r.watch(ctx, every, hup)
}

func (r *Reloader) watch(ctx context.Context, every time.Duration, hup <-chan os.Signal) {
	last := r.baseline
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			r.log.Infof("received SIGHUP, reloading config")
			last = r.stamp()
			_ = r.Reload()
		case <-tick.C:
			// A missing file mid-rename is not a change; wait for the next tick.
			if s, ok := r.statOK(); ok && s != last {
				last = s
				_ = r.Reload()
			}
		}
	}
}

type fileStamp struct {
	mod  time.Time
	size int64
	sum  [sha256.Size]byte
}

func (r *Reloader) statOK() (fileStamp, bool) {
	fi, err := os.Stat(r.path)
	if err != nil {
		return fileStamp{}, false
	}
	// Hash the content too: mtime+size alone miss a same-size edit written
	// within the filesystem clock's resolution (ext4 stamps from the coarse
	// clock, so two quick writes can share a timestamp).
	data, err := os.ReadFile(r.path)
	if err != nil {
		return fileStamp{}, false
	}
	return fileStamp{fi.ModTime(), fi.Size(), sha256.Sum256(data)}, true
}

func (r *Reloader) stamp() fileStamp {
	s, _ := r.statOK()
	return s
}
