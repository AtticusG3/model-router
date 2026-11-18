package router

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	"model-router/internal/config"
)

// Config is the running config.
func (r *Router) Config() *config.Config { return r.cfg() }

// SetReloader wires the config reload gate to /_router/reload and the status
// page. Call it before the HTTP server starts.
func (r *Router) SetReloader(rl *config.Reloader) { r.reloader = rl }

// Apply puts a validated config into service without restarting the process.
// See "Reloading config" in SPEC.md for the rules.
//
// The catalog (routing, pools, peers, filters, TTL, hold, slots) switches in
// one pointer store. A running backend is restarted only if its launch
// (command, env, ports) changed; a model dropped from the file is unloaded.
// Both wait for the model's in-flight requests first, and new requests for it
// queue until the replacement is up. If a restart fails, every change is
// rolled back to the previous config.
func (r *Router) Apply(next *config.Config) error {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	old := r.cfg()
	keepStartupOnly(next, old, r.logger)

	type kept struct {
		m   *Managed
		old *config.Stanza
	}
	var keep []kept
	var restart, remove []string
	r.mu.Lock()
	for key, m := range r.managed {
		if r.draining[key] {
			continue // an eviction or sweep is already unloading it
		}
		ns := next.Stanza(instanceModel(key))
		if n := instanceIndex(key); ns == nil || n < 0 || n >= ns.InstanceCount() {
			remove = append(remove, key)
		} else if os := m.stz(); !os.LaunchEqual(ns) {
			restart = append(restart, key)
		} else {
			keep = append(keep, kept{m, os})
			continue
		}
		// Claimed so no new request lands on it while the swap happens.
		r.draining[key] = true
	}
	r.mu.Unlock()
	sort.Strings(restart)
	sort.Strings(remove)

	r.cfgp.Store(next)
	for _, k := range keep {
		k.m.retarget(next.Stanza(instanceModel(k.m.id)))
	}

	var restore []string // restarted keys that were running: rollback brings them back
	for i, key := range restart {
		r.drainIdle(key)
		r.mu.Lock()
		m := r.managed[key]
		r.mu.Unlock()
		wasRunning := m != nil && m.State() == StateRunning
		_ = r.unloadInstance(key)
		if !wasRunning {
			continue // not serving: the next request starts it with the new launch
		}
		restore = append(restore, key)
		r.logger.Infof("config reload: launch of %s changed, restarting", key)
		if _, err := r.Load(key); err != nil {
			r.cfgp.Store(old)
			for _, k := range keep {
				k.m.retarget(k.old)
			}
			r.unclaim(restart[i+1:])
			r.unclaim(remove)
			for _, k := range restore {
				// The failed start opened a cooldown that would refuse the old launch.
				r.startHealthy(instanceModel(k))
				_ = r.unloadInstance(k)
				if _, err := r.Load(k); err != nil {
					r.logger.Errorf("config reload rollback: %s did not come back, it starts on the next request: %v", k, err)
				}
			}
			return fmt.Errorf("restart %s: %w (rolled back to the previous config)", key, err)
		}
	}
	for _, key := range remove {
		r.drainIdle(key)
		r.logger.Infof("config reload: %s removed from config, unloading", key)
		_ = r.unloadInstance(key)
	}
	return nil
}

// unclaim lifts the drain mark Apply put on keys it did not get to unload.
func (r *Router) unclaim(keys []string) {
	r.mu.Lock()
	for _, k := range keys {
		delete(r.draining, k)
	}
	r.mu.Unlock()
}

// drainIdle waits for key's in-flight requests to finish; a claimed key takes no new ones.
// shortcut: no deadline, so one stuck stream holds the reload, add an apply timeout if that bites.
func (r *Router) drainIdle(key string) {
	for r.occupancyOf(key) > 0 {
		select {
		case <-r.shutdown:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// keepStartupOnly carries the fields read once at startup (bound socket,
// access handler, GPU ledger, pollers) over from the running config.
func keepStartupOnly(next, old *config.Config, log *Logger) {
	if next.Listen != old.Listen || next.HeadroomMB != old.HeadroomMB ||
		!reflect.DeepEqual(next.Security, old.Security) || !reflect.DeepEqual(next.Telemetry, old.Telemetry) {
		log.Infof("config reload: listen, security, telemetry and headroom_mb need a restart; keeping the running values")
	}
	next.Listen, next.HeadroomMB, next.Security, next.Telemetry = old.Listen, old.HeadroomMB, old.Security, old.Telemetry
}
