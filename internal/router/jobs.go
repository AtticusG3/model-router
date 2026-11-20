package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// jobPollInterval is how often the router asks a backend (or peer) whether a
// job it holds a model for has ended. A var so tests can shorten it.
var jobPollInterval = 2 * time.Second

// maxJobSubmitBody caps how much of a submit response is held back to read
// the job id; anything larger is not a job record and passes straight through.
const maxJobSubmitBody = 1 << 20

// job is one async generation the router created through POST /v1/videos. It
// holds one occupancy slot on its target until the backend reports an end
// state, a DELETE ends it, or job_ttl_minutes passes. Fields after done are
// guarded by r.mu; the rest never change.
type job struct {
	id      string
	model   string // instance key or peer-local id, as proxyTo was given
	target  string // this node's name or the peer's name
	tgt     Target
	base    string // backend or peer base URL
	code    int    // HTTP status of the submit
	created time.Time
	done    bool
	state   string // last status the backend reported
}

// jobID returns the id in /v1/videos/{id} and /v1/videos/{id}/..., the
// follow-up calls that name no model; "" for any other path.
func jobID(path string) string {
	rest, ok := strings.CutPrefix(path, "/v1/videos/")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// proxyJobAware is proxyTo plus job bookkeeping: a 2xx answer to
// POST /v1/videos, or to a remix POST /v1/videos/{id}/remix (which makes a new
// job), records the job before the client sees its id.
func (r *Router) proxyJobAware(w http.ResponseWriter, req *http.Request, t Target, baseURL, model, target string) {
	if req.Method != http.MethodPost || (req.URL.Path != "/v1/videos" && !strings.HasSuffix(req.URL.Path, "/remix")) {
		r.proxyTo(w, req, baseURL, model, target)
		return
	}
	// The job outlives the submitting client, so its submit must too: a client
	// that drops mid-submit must not leave a job running that nothing holds.
	req = req.WithContext(context.WithoutCancel(req.Context()))
	hw := &holdBackWriter{ResponseWriter: w}
	r.proxyTo(hw, req, baseURL, model, target)
	if !hw.pass && hw.status()/100 == 2 {
		var v struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(hw.buf.Bytes(), &v) == nil && v.ID != "" {
			r.recordJob(&job{id: v.ID, model: model, target: target, tgt: t, base: baseURL, code: hw.status(), created: time.Now()})
		}
	}
	hw.release()
}

// recordJob adds j to the table, takes its occupancy slot and starts watching
// it. A full table drops the jobs that finished first; running ones are never
// dropped, so the table can run over the cap while every entry is running.
func (r *Router) recordJob(j *job) {
	limit := r.cfg().JobTableCap()
	r.mu.Lock()
	for len(r.jobs) >= limit && len(r.jobsDone) > 0 {
		o := r.jobsDone[0]
		r.jobsDone[0] = nil
		r.jobsDone = r.jobsDone[1:]
		if r.jobs[o.id] == o { // else a DELETE removed it or its id was reused
			delete(r.jobs, o.id)
		}
	}
	// Stale entries pile up when jobs are deleted and the table never fills;
	// prune them once they outnumber the table so the queue stays bounded.
	if len(r.jobsDone) > 2*max(len(r.jobs), limit) {
		r.jobsDone = slices.DeleteFunc(r.jobsDone, func(o *job) bool { return r.jobs[o.id] != o })
	}
	r.jobs[j.id] = j
	// A local submit still holds its own slot here, so the instance cannot be
	// draining and the count can be taken directly. A peer key never drains.
	r.occupancy[occupancyKey(j.tgt)]++
	r.mu.Unlock()
	r.logger.Infof("job %s on %s: submitted", j.id, occupancyKey(j.tgt))
	go r.watchJob(j)
}

// watchJob polls the job's status until it ends, then frees its hold.
func (r *Router) watchJob(j *job) {
	ttl := time.NewTimer(r.cfg().JobTTL())
	defer ttl.Stop()
	tick := time.NewTicker(jobPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-r.shutdown:
			return
		case <-ttl.C:
			r.endJob(j, "expired")
			return
		case <-tick.C:
		}
		r.mu.Lock()
		done := j.done
		lost := j.tgt.Local != "" && r.managed[j.tgt.Local] == nil
		r.mu.Unlock()
		switch {
		case done: // a DELETE ended it
			return
		case lost: // unloaded under the job; the job went with it
			r.endJob(j, "failed")
			return
		}
		state, err := r.pollJob(j)
		if err != nil {
			continue // a slow or restarting backend: try again until the TTL
		}
		switch state {
		case "completed", "failed", "cancelled", "gone":
			r.endJob(j, state)
			return
		}
		r.mu.Lock()
		j.state = state
		r.mu.Unlock()
	}
}

// pollJob asks the job's target for its status. A 404 is "gone".
func (r *Router) pollJob(j *job) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.base+"/v1/videos/"+url.PathEscape(j.id), nil)
	if err != nil {
		return "", err
	}
	if j.tgt.Peer != "" {
		req.Header.Set("X-Model-Router-Peer", r.node)
	}
	resp, err := r.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "gone", nil
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("job %s poll: %s", j.id, resp.Status)
	}
	var v struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJobSubmitBody)).Decode(&v); err != nil {
		return "", err
	}
	return v.Status, nil
}

// endJob frees the job's hold once and logs submit-to-end as one Activity row.
func (r *Router) endJob(j *job, state string) {
	r.mu.Lock()
	if j.done {
		r.mu.Unlock()
		return
	}
	j.done, j.state = true, state
	r.jobsDone = append(r.jobsDone, j)
	m := r.managed[j.tgt.Local]
	r.mu.Unlock()
	if m != nil {
		m.markUsed() // the idle clock starts when the job ends
	}
	r.releaseOccupancy(j.tgt)
	took := time.Since(j.created)
	r.logger.Infof("job %s on %s: %s after %s", j.id, occupancyKey(j.tgt), state, took.Round(time.Second))
	if r.activity != nil {
		r.activity.record(ActivityEntry{
			Timestamp: j.created, Model: j.model, Target: j.target,
			Method: http.MethodPost, Path: "/v1/videos", Status: j.code,
			DurationMs: int(took.Milliseconds()), JobID: j.id, JobState: state,
		}, ActivityCapture{}, nil)
	}
}

// serveJob sends a follow-up call (status, content, delete) to the instance or
// peer that owns the job. It never loads a model: an unknown id is 404.
func (r *Router) serveJob(w http.ResponseWriter, req *http.Request, id string) {
	r.mu.Lock()
	j := r.jobs[id]
	r.mu.Unlock()
	if j == nil {
		http.Error(w, fmt.Sprintf("unknown job %q: the router keeps jobs in memory only, so a restart forgets them; submit it again", id), http.StatusNotFound)
		return
	}
	if key := j.tgt.Local; key != "" {
		r.mu.Lock()
		m := r.managed[key]
		ok := m != nil && !r.draining[key] && m.State() == StateRunning
		if ok {
			r.occupancy[key]++
		}
		r.mu.Unlock()
		if !ok {
			http.Error(w, fmt.Sprintf("job %q: its model %s is no longer loaded", id, key), http.StatusNotFound)
			return
		}
		defer r.releaseOccupancy(j.tgt)
		m.markUsed()
		defer m.markUsed()
	} else {
		req.Header.Set("X-Model-Router-Peer", r.node)
	}
	if req.Method == http.MethodPost { // remix makes a child job, recorded and held like a submit
		r.proxyJobAware(w, req, j.tgt, j.base, j.model, j.target)
		return
	}
	if req.Method != http.MethodDelete {
		r.proxyTo(w, req, j.base, j.model, j.target)
		return
	}
	hw := &holdBackWriter{ResponseWriter: w}
	r.proxyTo(hw, req, j.base, j.model, j.target)
	if !hw.pass && hw.status()/100 == 2 {
		r.endJob(j, "cancelled")
		r.mu.Lock()
		if r.jobs[id] == j {
			delete(r.jobs, id)
		}
		r.mu.Unlock()
	}
	hw.release()
}

// RunningJobs counts jobs still holding a model, by model id.
func (r *Router) RunningJobs() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for _, j := range r.jobs {
		if !j.done {
			out[instanceModel(j.model)]++
		}
	}
	return out
}

// holdBackWriter keeps a small response from the client until the router has
// acted on it (recorded the job, or ended it on DELETE). A body over
// maxJobSubmitBody is released and the rest passes straight through.
type holdBackWriter struct {
	http.ResponseWriter
	code int
	buf  bytes.Buffer
	pass bool
}

func (h *holdBackWriter) WriteHeader(code int) {
	switch {
	case h.pass || code < 200: // interim heads go straight out
		h.ResponseWriter.WriteHeader(code)
	case h.code == 0:
		h.code = code
	}
}

func (h *holdBackWriter) Write(p []byte) (int, error) {
	if h.pass {
		return h.ResponseWriter.Write(p)
	}
	h.buf.Write(p)
	if h.buf.Len() > maxJobSubmitBody {
		h.release()
	}
	return len(p), nil
}

// Flush is a no-op while holding back: flushing would send the head early.
func (h *holdBackWriter) Flush() {
	if h.pass {
		_ = http.NewResponseController(h.ResponseWriter).Flush()
	}
}

func (h *holdBackWriter) status() int {
	if h.code == 0 {
		return http.StatusOK
	}
	return h.code
}

// release sends the held head and body, then passes everything through.
func (h *holdBackWriter) release() {
	if h.pass {
		return
	}
	h.pass = true
	h.ResponseWriter.WriteHeader(h.status())
	_, _ = h.ResponseWriter.Write(h.buf.Bytes())
}
