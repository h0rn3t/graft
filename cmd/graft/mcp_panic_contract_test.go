package main

import (
	"context"
	"strings"
	"testing"
)

// panicContext panics where a tool call first consults its context, standing
// in for a bug anywhere under the call.
type panicContext struct{ context.Context }

func (panicContext) Err() error { panic("boom under the tool call") }

func TestMCPCallContainsAPanic(t *testing.T) {
	root := t.TempDir()

	result := mcpCall(panicContext{t.Context()}, root, root, "", "graft_repo_map", nil)
	if !result.isError {
		t.Errorf("mcpCall with a panicking call: isError = false, want true")
	}
	for _, want := range []string{"graft_repo_map", "boom under the tool call", "mcp_panic_contract_test.go"} {
		if !strings.Contains(result.text, want) {
			t.Errorf("mcpCall with a panicking call: text lacks %q:\n%s", want, result.text)
		}
	}
	if len(result.text) > mcpPanicTraceBytes+512 {
		t.Errorf("mcpCall with a panicking call: %d bytes of text, want the trace cut near %d", len(result.text), mcpPanicTraceBytes)
	}

	// The next call on the same goroutine answers as if nothing had happened.
	next := mcpCall(t.Context(), root, root, "", "graft_repo_map", nil)
	if want := "no graph found — run `graft build` first"; next.text != want {
		t.Errorf("mcpCall after a contained panic = %q, want %q", next.text, want)
	}
}
