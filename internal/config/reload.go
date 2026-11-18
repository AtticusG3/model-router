package config

import (
	"context"
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
// and only a valid result reaches the apply hook. An invalid file leaves the
// running config alone and is recorded for the UI.
type Reloader struct {
	path  string
	apply func(*Config)
	log   Logger

	mu        sync.Mutex
	current   *Config
	lastErr   string
	lastErrAt time.Time
}

// NewReloader returns a reloader for path. current is the running config;
// apply receives each newly validated config.
func NewReloader(path string, current *Config, apply func(*Config), log Logger) *Reloader {
	return &Reloader{path: path, current: current, apply: apply, log: log}
}

// Reload validates the file and hands a valid config to the apply hook.
// The file and signal triggers both come through here.
func (r *Reloader) Reload() error {
	cfg, err := Load(r.path)
	r.mu.Lock()
	if err != nil {
		r.lastErr, r.lastErrAt = err.Error(), time.Now()
		r.mu.Unlock()
		// Parse errors name a line or field, never a value, so this is safe to log.
		r.log.Errorf("config reload rejected, keeping running config: %v", err)
		return err
	}
	r.lastErr, r.lastErrAt = "", time.Time{}
	r.current = cfg
	r.mu.Unlock()
	r.log.Infof("config reload accepted: %s", r.path)
	r.apply(cfg)
	return nil
}

// Current is the last config that passed validation (the initial one until then).
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

// Watch reloads on SIGHUP and whenever the file's mtime or size changes, until ctx ends.
// shortcut: polls os.Stat instead of fsnotify (no new dependency, survives editor rename-saves), upgrade if sub-second latency matters.
func (r *Reloader) Watch(ctx context.Context, every time.Duration) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	r.watch(ctx, every, hup)
}

func (r *Reloader) watch(ctx context.Context, every time.Duration, hup <-chan os.Signal) {
	last := r.stamp()
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
}

func (r *Reloader) statOK() (fileStamp, bool) {
	fi, err := os.Stat(r.path)
	if err != nil {
		return fileStamp{}, false
	}
	return fileStamp{fi.ModTime(), fi.Size()}, true
}

func (r *Reloader) stamp() fileStamp {
	s, _ := r.statOK()
	return s
}
