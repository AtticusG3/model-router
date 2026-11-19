package router

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Start cooldown windows are fixed on purpose: they are not config.
const (
	startCooldownBase = 10 * time.Second
	startCooldownMax  = 5 * time.Minute
)

// startCooldown is the per-model failed-start state. Every copy of a model
// shares it, so it is keyed by the stanza's model id, not the instance key.
type startCooldown struct {
	fails int       // consecutive failed starts
	until time.Time // no start of this model before this
}

// startCooldownError is a failed start, or a refusal while the window is open.
// The HTTP layer turns it into 503 with Retry-After.
type startCooldownError struct {
	cause error
	until time.Time
}

func (e *startCooldownError) Error() string {
	return fmt.Sprintf("%v (start cooldown, retry in %ds)", e.cause, e.retryAfter())
}

func (e *startCooldownError) Unwrap() error { return e.cause }

// retryAfter is the whole seconds still left, rounded up and never below 1.
func (e *startCooldownError) retryAfter() int {
	s := int((time.Until(e.until) + time.Second - 1) / time.Second)
	if s < 1 {
		return 1
	}
	return s
}

// startRefused returns the refusal for model while its window is open, else nil.
func (r *Router) startRefused(model string) error {
	r.coolMu.Lock()
	defer r.coolMu.Unlock()
	c := r.cooldowns[model]
	if c == nil || !time.Now().Before(c.until) {
		return nil
	}
	return &startCooldownError{cause: fmt.Errorf("model %s failed to start", model), until: c.until}
}

// startFailed opens the next window (10s, doubling, capped at 5m) and returns
// err wrapped so the client sees the window.
func (r *Router) startFailed(model string, err error) error {
	r.coolMu.Lock()
	defer r.coolMu.Unlock()
	c := r.cooldowns[model]
	if c == nil {
		c = &startCooldown{}
		r.cooldowns[model] = c
	}
	c.fails++
	window := startCooldownBase
	for i := 1; i < c.fails && window < startCooldownMax; i++ {
		window *= 2
	}
	window = min(window, startCooldownMax)
	c.until = time.Now().Add(window)
	return &startCooldownError{cause: err, until: c.until}
}

// startHealthy resets the next window to the base.
func (r *Router) startHealthy(model string) {
	r.coolMu.Lock()
	delete(r.cooldowns, model)
	r.coolMu.Unlock()
}

// admitRetryAfter is the Retry-After (seconds) sent when a request found no
// room to run.
const admitRetryAfter = 15

// admitError is a request that found no GPU room (pinned models hold it, or
// the wait ran out). The HTTP layer turns it into 503 with Retry-After.
type admitError struct{ msg string }

func (e *admitError) Error() string { return e.msg }

// loadFailed answers a failed load: 503, with Retry-After when a start
// cooldown or a lack of room is behind it.
func loadFailed(w http.ResponseWriter, err error) {
	var cd *startCooldownError
	var ae *admitError
	if errors.As(err, &cd) {
		w.Header().Set("Retry-After", strconv.Itoa(cd.retryAfter()))
	} else if errors.As(err, &ae) {
		w.Header().Set("Retry-After", strconv.Itoa(admitRetryAfter))
	}
	http.Error(w, err.Error(), http.StatusServiceUnavailable)
}
