package router

import (
	"reflect"
	"testing"
)

func TestSplitCommandQuotes(t *testing.T) {
	got := splitCommand(`/opt/ai/bin/llama-server --model /m.gguf --reasoning-budget-message "[[SYSTEM] stop now]"`)
	want := []string{
		"/opt/ai/bin/llama-server",
		"--model",
		"/m.gguf",
		"--reasoning-budget-message",
		"[[SYSTEM] stop now]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestSplitCommandHealthProxy(t *testing.T) {
	got := splitCommand(`/opt/ai/bin/sd-health-proxy 5915 -- /home/kevyn/infra/stable-diffusion.cpp/build/bin/sd-server --steps 8`)
	if got[0] != "/opt/ai/bin/sd-health-proxy" || got[1] != "5915" || got[2] != "--" {
		t.Fatalf("proxy argv = %#v", got)
	}
}
