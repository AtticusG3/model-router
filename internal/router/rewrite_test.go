package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
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
	if err := rewriteBodyModel(req, newModel, field); err != nil {
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

func TestRewriteBodyModelQuery(t *testing.T) {
	req := rewriteBody(t, "GET", "http://x/props?model=digger/coding-model", "",
		"", "coding-model", "model_name")
	if req.URL.Query().Get("model") != "coding-model" {
		t.Errorf("query model = %q, want coding-model", req.URL.Query().Get("model"))
	}
}
