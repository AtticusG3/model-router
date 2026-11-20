package router

import (
	"context"
	"errors"
	"slices"
)

// notePreloadEvicted remembers that a preload model lost its VRAM to evictor,
// so restorePreloads can start it again once the room comes back. A preload
// the operator unloads itself never passes through here, so it stays down.
func (r *Router) notePreloadEvicted(id, evictor string) {
	if !slices.Contains(r.cfg().Preload, id) {
		return
	}
	r.mu.Lock()
	r.evictedPreloads[id] = instanceModel(evictor)
	r.mu.Unlock()
}

func (r *Router) clearPreloadEviction(id string) {
	r.mu.Lock()
	delete(r.evictedPreloads, id)
	r.mu.Unlock()
}

// restorePreloads starts preload models that an eviction pushed out, in
// config order, once each one fits without evicting anything. It never
// evicts, never waits, and never fights a load already in flight: a stanza
// the operator unloaded, or one the reload dropped from preload, is not in
// the pending set or not in the config and stays down. A model with no room
// yet keeps its mark and is retried on the next free.
func (r *Router) restorePreloads() {
	select {
	case <-r.shutdown:
		return
	default:
	}
	if !r.restoreMu.TryLock() {
		return
	}
	defer r.restoreMu.Unlock()
	cfg := r.cfg()
	for _, id := range cfg.Preload {
		r.mu.Lock()
		evictor, pending := r.evictedPreloads[id]
		m := r.managed[id]
		draining := r.draining[id]
		state := StateStopped
		if m != nil {
			state = m.State()
		}
		r.mu.Unlock()
		if !pending {
			continue
		}
		s := cfg.Stanza(id)
		if s == nil || state == StateRunning {
			// Gone from the catalog, or already up: nothing to restore.
			r.clearPreloadEviction(id)
			continue
		}
		if draining || state == StateStarting || state == StateStopping {
			continue // mid-transition; leave the mark for a later pass
		}
		if r.startRefused(s.ModelID) != nil || !r.ledger.CanAdmit(id, s) {
			continue // backing off, or no room yet
		}
		r.logger.Infof("restoring preload %s after eviction by %s", id, evictor)
		if _, err := r.load(context.Background(), id, false, false); err != nil {
			// A room error keeps the mark so a later free retries; anything
			// else means its next request has to start it.
			var ae *admitError
			if !errors.As(err, &ae) {
				r.clearPreloadEviction(id)
			}
			r.logger.Errorf("restore preload %s failed: %v", id, err)
			continue
		}
		r.clearPreloadEviction(id)
	}
}
