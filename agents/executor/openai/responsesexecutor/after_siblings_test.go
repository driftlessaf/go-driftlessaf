/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

// TestAfterSiblingsToolRunsAfterItsTurn: in a turn of submit, check and
// write, listed in that order, the check (AfterSiblings) runs after the write
// and the submission's validator after the check. With one call at a time,
// slot order would run the check first, and a check held with the submit
// would run after it.
func TestAfterSiblingsToolRunsAfterItsTurn(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}
	cfg := config(t)
	cfg.ToolCallConcurrency = 1
	cfg.ResultValidators = []callbacks.ResultValidator[answer]{func(context.Context, answer, string) ([]callbacks.Finding, error) {
		record("submit")
		return nil, nil
	}}
	e := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		emit(w, submit("s", "done"), call("check", "check", `{}`), call("write", "write", `{}`))
	}, cfg)

	tool := func(name string, after bool) toolcall.Tool[answer] {
		return toolcall.Tool[answer]{
			Def: toolcall.Definition{Name: name, Description: name, AfterSiblings: after},
			Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[answer], *answer) map[string]any {
				record(name)
				return map[string]any{"ok": true}
			},
		}
	}
	tools := map[string]toolcall.Tool[answer]{"check": tool("check", true), "write": tool("write", false)}

	got, err := e.Execute(t.Context(), request{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.Text != "done" {
		t.Errorf("result: got = %q, want = %q", got.Text, "done")
	}
	if want := []string{"write", "check", "submit"}; !slices.Equal(order, want) {
		t.Errorf("run order: got = %v, want = %v", order, want)
	}
}
