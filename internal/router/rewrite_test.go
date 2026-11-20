package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-router/internal/config"
)

func rewriteBody(t *testing.T, method, target, ct, body, newModel, field string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if err := rewriteBodyModel(req, newModel, field, config.Filters{}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	return req
}

func TestRewriteBodyModelJSON(t *testing.T) {
	req := rewriteBody(t, "POST", "http://x/v1/chat/completions", "application/json",
		`{"model":"digger/coding-model","messages":[]}`, "coding-model", "model")
	var obj map[string]any
	raw, _ := io.ReadAll(req.Body)
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal rewritten body: %v", err)
	}
	if obj["model"] != "coding-model" {
		t.Errorf("model = %v, want coding-model", obj["model"])
	}
	if _, ok := obj["messages"]; !ok {
		t.Error("messages field was dropped")
	}
}

func TestRewriteBodyModelCustomField(t *testing.T) {
	req := rewriteBody(t, "POST", "http://x/v1/chat/completions", "application/json",
		`{"model":"digger/coding-model"}`, "coding-model", "model_name")
	var obj map[string]any
	raw, _ := io.ReadAll(req.Body)
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal rewritten body: %v", err)
	}
	if obj["model_name"] != "coding-model" {
		t.Errorf("model_name = %v, want coding-model", obj["model_name"])
	}
}

func TestRewriteBodyModelUrlencoded(t *testing.T) {
	req := rewriteBody(t, "POST", "http://x/v1/audio/transcriptions", "application/x-www-form-urlencoded",
		"model=far%2Fwhisper-x&language=en", "whisper-x", "model")
	if got, _ := io.ReadAll(req.Body); string(got) != "language=en&model=whisper-x" || req.ContentLength != int64(len(got)) {
		t.Errorf("body = %q, content length %d", got, req.ContentLength)
	}
}

// A body that is neither JSON nor a form (or has no Content-Type) is forwarded
// as sent.
func TestRewriteBodyModelLeavesRawAlone(t *testing.T) {
	for _, ct := range []string{"application/octet-stream", "text/plain", ""} {
		req := rewriteBody(t, "POST", "http://x/v1/audio/transcriptions", ct, "RIFF", "whisper-x", "model")
		if got, _ := io.ReadAll(req.Body); string(got) != "RIFF" || req.ContentLength != 4 {
			t.Errorf("content type %q: body = %q, content length %d", ct, got, req.ContentLength)
		}
	}
}

// A multipart transcription for peer/model reaches the peer with the peer's
// own id (in the model part, and in a ?model= query the peer's matcher would
// read first), and the streamed file part arrives byte-identical. A body with
// no closing boundary is the client's fault: 400, not a 502 upstream error.
func TestRewriteMultipartPeer(t *testing.T) {
	var got, gotCT, gotQuery string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if req.URL.Path != "/_router/load" {
			got, gotCT, gotQuery = string(b), req.Header.Get("Content-Type"), req.URL.Query().Get("model")
		}
		_, _ = w.Write([]byte(`{"text":"fictional"}`))
	}))
	defer peer.Close()
	cfg, err := config.Parse([]byte(`
peers:
  - name: far
    base_url: ` + peer.URL + `
    models: [whisper-x]
`))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(New(cfg, NewLogger(io.Discard, false), "test"), NewLogger(io.Discard, false))
	withModel := transcriptionBody(t, [2]string{"model", "far/whisper-x"}, [2]string{"language", "en"})
	for _, tc := range []struct {
		name, query, body string
		want              int
	}{
		{"model part", "", withModel, http.StatusOK},
		{"query model", "?model=far/whisper-x", transcriptionBody(t, [2]string{"language", "en"}), http.StatusOK},
		// Last: its peer handler may still be running when the proxy answers.
		{"no closing boundary", "", strings.TrimSuffix(withModel, "--fictionalboundary--\r\n"), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, gotCT, gotQuery = "", "", ""
			req := httptest.NewRequest("POST", "/v1/audio/transcriptions"+tc.query, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", transcriptionCT)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK {
				return
			}
			if gotCT != transcriptionCT {
				t.Errorf("peer content type %q", gotCT)
			}
			if tc.query != "" && gotQuery != "whisper-x" {
				t.Errorf("peer query model = %q, want whisper-x", gotQuery)
			}
			if f := formFields(t, got); f["model"] != "whisper-x" || f["language"] != "en" {
				t.Errorf("peer fields = %v", f)
			}
		})
	}
}

// A spilled multipart request that named an alias reaches the peer with the
// stanza id.
func TestRewriteMultipartAliasOnSpill(t *testing.T) {
	p := newWalkPeer(t, http.StatusOK, "")
	r := walkRouter(t, "", p)
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions",
		strings.NewReader(transcriptionBody(t, [2]string{"model", "shared-alias"})))
	req.Header.Set("Content-Type", transcriptionCT)
	if !r.serveSpill(httptest.NewRecorder(), req, "shared-model") {
		t.Fatal("spill should reach the peer")
	}
	body, _ := p.chatBody.Load().(string)
	if f := formFields(t, body); f["model"] != "shared-model" {
		t.Errorf("peer fields = %v, want model=shared-model", f)
	}
}

func TestRewriteBodyModelQuery(t *testing.T) {
	req := rewriteBody(t, "GET", "http://x/props?model=digger/coding-model", "",
		"", "coding-model", "model_name")
	if req.URL.Query().Get("model") != "coding-model" {
		t.Errorf("query model = %q, want coding-model", req.URL.Query().Get("model"))
	}
}

// A GET is not special: its query and its body (here JSON) are both rewritten.
func TestRewriteBodyModelGetWithBody(t *testing.T) {
	req := rewriteBody(t, "GET", "http://x/props?model=digger/coding-model", "application/json",
		`{"model":"digger/coding-model"}`, "coding-model", "model")
	if got, _ := io.ReadAll(req.Body); req.URL.Query().Get("model") != "coding-model" || string(got) != `{"model":"coding-model"}` {
		t.Errorf("query model = %q, body = %s", req.URL.Query().Get("model"), got)
	}
}
