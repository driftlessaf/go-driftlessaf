/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestAfterSiblingsToolRunsAfterItsTurn: in a turn of submit, check and
// write, listed in that order, the check (AfterSiblings) runs after the write
// and the submission's validator after the check. With one call at a time,
// slot order would run the check first, and a check held with the submit
// would run after it.
func TestAfterSiblingsToolRunsAfterItsTurn(t *testing.T) {
	srv := newValidatingAnthropicServer(t, func(int, []byte) []string {
		blocks := toolUseBlockSSE(t, 0, "toolu_submit", "submit_result", submitInput("done"))
		blocks = append(blocks, toolUseBlockSSE(t, 1, "toolu_check", "check", map[string]any{"reasoning": "r"})...)
		blocks = append(blocks, toolUseBlockSSE(t, 2, "toolu_write", "write", map[string]any{"reasoning": "r"})...)
		return wrapTurn("msg_01", blocks...)
	})

	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}
	tool := func(name string, after bool) claudetool.Metadata[errCapResponse] {
		return claudetool.FromTool(toolcall.Tool[errCapResponse]{
			Def: toolcall.Definition{Name: name, Description: name, AfterSiblings: after},
			Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				record(name)
				return map[string]any{"ok": true}
			},
		})
	}
	tools := map[string]claudetool.Metadata[errCapResponse]{"check": tool("check", true), "write": tool("write", false)}

	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)),
		prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
		claudeexecutor.WithToolCallConcurrency[errCapRequest, errCapResponse](1),
		claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
		claudeexecutor.WithResultValidator[errCapRequest, errCapResponse](func(context.Context, errCapResponse, string) ([]callbacks.Finding, error) {
			record("submit")
			return nil, nil
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "done"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if want := []string{"write", "check", "submit"}; !slices.Equal(order, want) {
		t.Errorf("run order: got = %v, want = %v", order, want)
	}
}
