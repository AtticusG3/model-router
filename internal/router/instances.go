package router

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"model-router/internal/config"
)

// instanceKey names backend n of a model. Instance 0 keeps the bare model id
// so single-instance stanzas, logs, and /_router/unload look as before.
func instanceKey(modelID string, n int) string {
	if n <= 0 {
		return modelID
	}
	return fmt.Sprintf("%s#%d", modelID, n)
}

// instanceModel strips the "#n" suffix from an instance key.
func instanceModel(key string) string {
	id, _, _ := strings.Cut(key, "#")
	return id
}

func instanceIndex(key string) int {
	_, n, ok := strings.Cut(key, "#")
	if !ok {
		return 0
	}
	i, err := strconv.Atoi(n)
	if err != nil {
		return -1
	}
	return i
}

// stanzaOf resolves an instance key, model id, or alias to its stanza.
func (r *Router) stanzaOf(key string) *config.Stanza {
	id := instanceModel(key)
	if s := r.cfg.Stanza(id); s != nil {
		return s
	}
	return r.cfg.Alias(id)
}

// managedLocked returns the supervisor for key, creating it on the instance's
// own port. Caller holds r.mu.
func (r *Router) managedLocked(key string, s *config.Stanza) *Managed {
	if m, ok := r.managed[key]; ok {
		return m
	}
	n := instanceIndex(key)
	port := s.InstancePort(n)
	m := newManaged(s, port, r.logger)
	m.id = key
	if n > 0 {
		m.proxyURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	m.shutdown = r.shutdown
	if r.spawn != nil {
		m.spawn = r.spawn
	}
	r.managed[key] = m
	return m
}

// liveInstancesLocked lists the keys of s's instances that exist and are not
// being unloaded, in index order. Caller holds r.mu.
func (r *Router) liveInstancesLocked(s *config.Stanza) []string {
	var out []string
	for n := 0; n < s.InstanceCount(); n++ {
		key := instanceKey(s.ModelID, n)
		if (r.managed[key] != nil || r.occupancy[key] > 0) && !r.draining[key] {
			out = append(out, key)
		}
	}
	return out
}

// hasLiveSibling reports whether another instance of key's model is up or
// starting. Extra copies never evict or wait for VRAM: the model is already
// served, so a missing GPU just means the request queues on a sibling.
func (r *Router) hasLiveSibling(key string) bool {
	s := r.stanzaOf(key)
	if s == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range r.liveInstancesLocked(s) {
		if k != key {
			return true
		}
	}
	return false
}

// acquireInstance picks the backend of s that serves the next request and
// holds an occupancy slot on it. Preference: a running instance with a free
// slot, a starting one with a free slot, then a new instance. The first
// instance may evict to fit (load does that); extra copies are only started
// on a GPU that fits them without evicting anything. With queue, a fully busy
// model falls back to its least-loaded instance; without, false means every
// instance is busy. Instances being unloaded are waited out, not reused.
func (r *Router) acquireInstance(s *config.Stanza, queue bool) (string, bool) {
	for {
		r.mu.Lock()
		key, ok, retry := r.acquireLocked(s, queue)
		r.mu.Unlock()
		if ok || !retry {
			return key, ok
		}
		select {
		case <-r.shutdown:
			return "", false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (r *Router) acquireLocked(s *config.Stanza, queue bool) (key string, ok, retry bool) {
	slots := s.SlotCount()
	var running, starting, least, free string
	runOcc, startOcc, leastOcc := slots, slots, 0
	live, draining := 0, false
	for n := 0; n < s.InstanceCount(); n++ {
		k := instanceKey(s.ModelID, n)
		if r.draining[k] {
			draining = true
			continue
		}
		m := r.managed[k]
		occ := r.occupancy[k]
		// A held slot with no supervisor yet is a request about to load it.
		if m == nil && occ == 0 {
			if free == "" {
				free = k
			}
			continue
		}
		live++
		if m != nil && m.State() == StateRunning {
			if occ < runOcc {
				running, runOcc = k, occ
			}
		} else if occ < startOcc {
			starting, startOcc = k, occ
		}
		if least == "" || occ < leastOcc {
			least, leastOcc = k, occ
		}
	}
	pick := running
	if pick == "" {
		pick = starting
	}
	if pick == "" && free != "" {
		switch {
		case live == 0 && !draining:
			pick = free
		case live > 0:
			if _, fits := r.ledger.Reserve(free, s); fits {
				r.logger.Infof("all %d instance(s) of %s busy: starting %s", live, s.ModelID, free)
				pick = free
			}
		}
	}
	if pick == "" && queue {
		pick = least
	}
	if pick == "" {
		return "", false, queue
	}
	r.managedLocked(pick, s)
	r.occupancy[pick]++
	return pick, true, false
}
