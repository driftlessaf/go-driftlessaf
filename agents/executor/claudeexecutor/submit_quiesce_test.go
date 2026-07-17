/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// slowToolAndSubmitTurn renders the SSE events for an assistant turn that
// calls slow_tool and submit_result in parallel (two tool_use blocks).
func slowToolAndSubmitTurn(t *testing.T, input map[string]any) []string {
	t.Helper()
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	partial, err := json.Marshal(string(inputJSON))
	if err != nil {
		t.Fatalf("marshal partial: %v", err)
	}
	return []string{
		`{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_slow","name":"slow_tool","input":{}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_submit","name":"submit_result","input":{}}}`,
		fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":%s}}`, partial),
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}
}

// TestSubmitEvaluatedAfterToolHandlers pins the quiesce guarantee documented
// on callbacks.ResultValidator: when a turn carries a submit call alongside
// other tool calls, the submission's validators run only after those
// handlers have completed — so validators that read state the handlers
// produce (worktrees, files) observe the finished state instead of racing
// them under the concurrent tool pool.
func TestSubmitEvaluatedAfterToolHandlers(t *testing.T) {
	// Drive the turn through the validating server so the submit + slow_tool
	// turn is additionally checked for tool_use/tool_result pairing and the
	// cache-breakpoint budget.
	srv := newValidatingAnthropicServer(t, func(int, []byte) []string {
		return slowToolAndSubmitTurn(t, submitInput("done"))
	})

	// validatorStarted lets the tool handler observe a validator that began
	// while the handler was still running — the exact interleaving the
	// quiesce guarantee forbids — so regression detection rides the channel,
	// not the clock. Under the guarantee the validator never starts first,
	// the handler's wait times out, and the timeout bounds only the fixed
	// path's wall time.
	validatorStarted := make(chan struct{})
	var handlerDone, validatorRanEarly, validatorSawDone atomic.Bool
	validator := func(_ context.Context, _ errCapResponse, _ string) ([]callbacks.Finding, error) {
		close(validatorStarted)
		validatorSawDone.Store(handlerDone.Load())
		return nil, nil
	}

	exec := newSubmitExecutor(t, srv, validator)
	tools := map[string]claudetool.Metadata[errCapResponse]{
		"slow_tool": {
			Definition: anthropic.ToolParam{Name: "slow_tool"},
			Handler: func(_ context.Context, _ anthropic.ToolUseBlock, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) map[string]any {
				select {
				case <-validatorStarted:
					validatorRanEarly.Store(true)
				case <-time.After(250 * time.Millisecond):
				}
				handlerDone.Store(true)
				return map[string]any{"ok": true}
			},
		},
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "done"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if validatorRanEarly.Load() {
		t.Error("validator started while the turn's other tool handler was still running")
	}
	if !validatorSawDone.Load() {
		t.Error("validator ran before the turn's other tool handlers completed")
	}
}
