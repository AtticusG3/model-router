package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"model-router/internal/config"
)

// A decision stanza is a passthrough of the backend's own System One API
// (ADR 0004): the router checks the envelope on the way in and the reply's
// shape on the way out, and never rewrites either body.

// systemOnePath is the route the decision contract covers. Kev's /permute and
// /separate variants pass through unchecked.
const systemOnePath = "/v1/systemone"

// maxDecisionReply caps the reply buffered for the shape check. A System One
// reply is a few KB; anything near this is not one.
const maxDecisionReply = 1 << 20

// isSystemOne reports whether req is a System One call on decision stanza s.
func isSystemOne(req *http.Request, s *config.Stanza) bool {
	return s != nil && s.APIType == config.APITypeDecision && req.Method == http.MethodPost && req.URL.Path == systemOnePath
}

// checkSystemOneRequest checks the System One envelope: a JSON object with a
// state and at least one named question, each with a type. What the state and
// questions say is the backend's business. The body is put back unchanged.
func checkSystemOneRequest(req *http.Request, model string) error {
	bad := func(why string) error { return fmt.Errorf("decision request for %s: %s", model, why) }
	if !strings.Contains(req.Header.Get("Content-Type"), "application/json") {
		return bad("Content-Type must be application/json")
	}
	var raw []byte
	if req.Body != nil {
		var err error
		if raw, err = io.ReadAll(req.Body); err != nil {
			return err
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
	}
	var env struct {
		State     json.RawMessage `json:"state"`
		Questions json.RawMessage `json:"questions"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return bad("body must be a JSON object")
	}
	if len(env.State) == 0 || string(env.State) == "null" {
		return bad("state is required")
	}
	var qs map[string]struct {
		Type any `json:"type"`
	}
	if json.Unmarshal(env.Questions, &qs) != nil || len(qs) == 0 {
		return bad("questions must be an object of named questions")
	}
	for name, q := range qs {
		if t, _ := q.Type.(string); t == "" {
			return bad(fmt.Sprintf("question %q needs a type", name))
		}
	}
	return nil
}

// decisionReplyKey carries the model id whose System One reply proxyToWriter
// checks.
type decisionReplyKey struct{}

// checkDecisionReply turns a 2xx reply that is not a System One answer set
// into an error, which the proxy answers as a 502 naming the model. A good
// reply goes on byte for byte; any other status passes untouched.
func checkDecisionReply(model string) func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp.StatusCode/100 != 2 {
			return nil
		}
		// The Transport decodes only the gzip it asked for; any other encoding
		// cannot be shape-checked, so it passes through untouched.
		if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
			return nil
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDecisionReply+1))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("%s decision reply: %w", model, err)
		}
		var reply struct {
			Answers map[string]json.RawMessage `json:"answers"`
		}
		if len(raw) > maxDecisionReply || json.Unmarshal(raw, &reply) != nil || reply.Answers == nil {
			return fmt.Errorf("%s returned a malformed decision reply (want a JSON object with an answers object)", model)
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
}

// DecisionStats summarises a decision row for the Activity page.
type DecisionStats struct {
	Questions int `json:"questions"`
	// Outcome is "name=answer" per answer, by name: the choice, score or noul
	// value exactly as the model wrote it.
	Outcome string `json:"outcome,omitempty"`
	// Confidence is the lowest confidence any answer reported; nil when none
	// did (a noul answer carries its probability instead).
	Confidence *float64 `json:"confidence,omitempty"`
}

// decisionStats reads a row's question count from the request and its outcome
// from a System One reply; nil when the reply has no answers.
func decisionStats(reqBody, respBody []byte) *DecisionStats {
	var reply struct {
		Answers map[string]map[string]json.RawMessage `json:"answers"`
	}
	if json.Unmarshal(respBody, &reply) != nil || reply.Answers == nil {
		return nil
	}
	var in struct {
		Questions map[string]json.RawMessage `json:"questions"`
	}
	_ = json.Unmarshal(reqBody, &in)
	d := &DecisionStats{Questions: len(in.Questions)}
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(reply.Answers)) {
		a := reply.Answers[name]
		var typ string
		_ = json.Unmarshal(a["type"], &typ)
		for _, k := range []string{"choice", "score", "noul"} { // no usable type: the first value present
			if a[typ] == nil {
				typ = k
			}
		}
		parts = append(parts, name+"="+strings.Trim(string(a[typ]), `"`))
		var c *float64 // a JSON null stays nil: absent, not 0
		if json.Unmarshal(a["confidence"], &c) == nil && c != nil && (d.Confidence == nil || *c < *d.Confidence) {
			d.Confidence = c
		}
	}
	d.Outcome = clipRunes(strings.Join(parts, ", "), 200)
	return d
}
