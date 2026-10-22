/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

const promptBudgetNudge = "close to the model's context limit"

// promptSizedToolTurn is a turn that calls tool after being sent a prompt of
// uncached + cacheRead + cacheWrite input tokens.
func promptSizedToolTurn(t *testing.T, n int, tool string, input map[string]any, uncached, cacheRead, cacheWrite int) []string {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := json.Marshal(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_%d","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":1}}}`, n, uncached, cacheRead, cacheWrite),
		fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_%d","name":%q,"input":{}}}`, n, tool),
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, partial),
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}
}

// TestPromptTokensBeforeFinalize pins when the prompt-budget nudge reaches the
// model: only once the next request, counting cached input and the turn's
// output (5 tokens here), reaches the threshold, and then with the submit tool
// forced on the next request.
func TestPromptTokensBeforeFinalize(t *testing.T) {
	for _, tc := range []struct {
		name      string
		threshold int64
		// The first turn's prompt split into uncached, cache-read and
		// cache-written input tokens.
		uncached, cacheRead, cacheWrite int
		wantNudge                       bool
	}{
		{name: "cached input counts toward the threshold", threshold: 900, uncached: 100, cacheRead: 750, cacheWrite: 50, wantNudge: true},
		{name: "the turn's output counts toward the threshold", threshold: 900, uncached: 100, cacheRead: 750, cacheWrite: 46, wantNudge: true},
		{name: "below the threshold sends nothing", threshold: 900, uncached: 100, cacheRead: 744, cacheWrite: 50},
		{name: "zero threshold is off", threshold: 0, uncached: 100, cacheRead: 750_000, cacheWrite: 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, requests := newExecTestClient(t, func(n int) []string {
				if n == 1 {
					return promptSizedToolTurn(t, n, "lookup", map[string]any{"q": "x"}, tc.uncached, tc.cacheRead, tc.cacheWrite)
				}
				return promptSizedToolTurn(t, n, "submit_result", submitInput("done"), 10, 0, 0)
			})
			prompt, err := promptbuilder.NewPrompt("Search the repository.")
			if err != nil {
				t.Fatal(err)
			}
			exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt,
				claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](4),
				claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
				claudeexecutor.WithPromptTokensBeforeFinalize[errCapRequest, errCapResponse](tc.threshold),
				claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := exec.Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{
				"lookup": {
					Definition: anthropic.ToolParam{Name: "lookup", InputSchema: anthropic.ToolInputSchemaParam{Type: "object"}},
					Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
						return map[string]any{"result": "evidence"}
					},
				},
			})
			if err != nil || result.Answer != "done" {
				t.Fatalf("Execute: result = %+v, err = %v, want the submitted result", result, err)
			}

			reqs := requests()
			if len(reqs) != 2 {
				t.Fatalf("requests: got = %d, want = 2", len(reqs))
			}
			var second struct {
				ToolChoice struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"tool_choice"`
			}
			if err := json.Unmarshal(reqs[1], &second); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(reqs[1]), promptBudgetNudge); got != tc.wantNudge {
				t.Errorf("second request carries the nudge: got = %v, want = %v", got, tc.wantNudge)
			}
			if got := second.ToolChoice.Type == "tool" && second.ToolChoice.Name == "submit_result"; got != tc.wantNudge {
				t.Errorf("second request forces submit_result: got = %v (tool_choice %+v), want = %v", got, second.ToolChoice, tc.wantNudge)
			}
		})
	}
}

// promptBudgetRun drives an executor with a lookup tool and a validator that
// rejects every answer but "done", serving turns from the script, and returns
// the request bodies it sent.
func promptBudgetRun(t *testing.T, script func(n int) []string, opts ...claudeexecutor.Option[errCapRequest, errCapResponse]) []string {
	t.Helper()
	client, requests := newExecTestClient(t, script)
	prompt, err := promptbuilder.NewPrompt("Search the repository.")
	if err != nil {
		t.Fatal(err)
	}
	opts = append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](8),
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
		claudeexecutor.WithResultValidator[errCapRequest, errCapResponse](func(_ context.Context, r errCapResponse, _ string) ([]callbacks.Finding, error) {
			if r.Answer != "done" {
				return []callbacks.Finding{{Kind: callbacks.FindingKindReview, Identifier: "wrong", Details: "Try again."}}, nil
			}
			return nil, nil
		}),
	}, opts...)
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{
		"lookup": {
			Definition: anthropic.ToolParam{Name: "lookup", InputSchema: anthropic.ToolInputSchemaParam{Type: "object"}},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"result": "evidence"}
			},
		},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	bodies := make([]string, 0, len(requests()))
	for _, r := range requests() {
		bodies = append(bodies, string(r))
	}
	return bodies
}

// forcedTool reports whether a request body forces a tool, and which.
func forcedTool(t *testing.T, body string) (name string, forced bool) {
	t.Helper()
	var req struct {
		ToolChoice struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return req.ToolChoice.Name, req.ToolChoice.Type == "tool"
}

// forcesSubmit reports whether a request body forces the submit_result tool.
func forcesSubmit(t *testing.T, body string) bool {
	name, forced := forcedTool(t, body)
	return forced && name == "submit_result"
}

// forcesATool reports whether a request body forces any tool.
func forcesATool(t *testing.T, body string) bool {
	_, forced := forcedTool(t, body)
	return forced
}

// TestPromptBudgetNudgeRepeatsWhileOverBudget pins what keeps a run inside the
// window after its forced submission is rejected: every later tool turn over
// budget adds the instruction again, and only the first is forced, so the
// model can still call the tools a rejection asks for.
func TestPromptBudgetNudgeRepeatsWhileOverBudget(t *testing.T) {
	bodies := promptBudgetRun(t, func(n int) []string {
		switch n {
		case 1:
			return promptSizedToolTurn(t, n, "lookup", map[string]any{"q": "x"}, 10, 1000, 0)
		case 2:
			return promptSizedToolTurn(t, n, "submit_result", submitInput("wrong"), 10, 1100, 0)
		case 3:
			return promptSizedToolTurn(t, n, "lookup", map[string]any{"q": "y"}, 10, 1200, 0)
		default:
			return promptSizedToolTurn(t, n, "submit_result", submitInput("done"), 10, 1300, 0)
		}
	}, claudeexecutor.WithPromptTokensBeforeFinalize[errCapRequest, errCapResponse](500))

	if len(bodies) != 4 {
		t.Fatalf("requests: got = %d, want = 4", len(bodies))
	}
	for i, want := range []struct {
		nudges int
		forced bool
	}{{0, false}, {1, true}, {2, false}, {3, false}} {
		if got := strings.Count(bodies[i], promptBudgetNudge); got != want.nudges {
			t.Errorf("request %d prompt-budget instructions: got = %d, want = %d", i+1, got, want.nudges)
		}
		if got := forcesSubmit(t, bodies[i]); got != want.forced {
			t.Errorf("request %d forces submit_result: got = %v, want = %v", i+1, got, want.forced)
		}
	}
}

// TestPromptBudgetAndToolCallCap pins how the two thresholds share the forced
// turn: a prompt over budget wins a tie, and once the tool-call cap has forced
// submit, a later prompt over budget sends its instruction unforced.
func TestPromptBudgetAndToolCallCap(t *testing.T) {
	const toolCapNudge = "You have gathered enough evidence"
	for _, tc := range []struct {
		name string
		// Prompt sizes of the two lookup turns before the run submits.
		first, second int
		// Wanted on requests 2 and 3.
		wantPrompt, wantToolCap [2]int
		wantForced              [2]bool
	}{
		{name: "tie goes to the prompt budget", first: 1000, second: 1000,
			wantPrompt: [2]int{1, 2}, wantToolCap: [2]int{0, 0}, wantForced: [2]bool{true, false}},
		{name: "tool-call cap first, then the prompt budget", first: 100, second: 1000,
			wantPrompt: [2]int{0, 1}, wantToolCap: [2]int{1, 1}, wantForced: [2]bool{true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodies := promptBudgetRun(t, func(n int) []string {
				switch n {
				case 1:
					return promptSizedToolTurn(t, n, "lookup", map[string]any{"q": "x"}, 10, tc.first, 0)
				case 2:
					return promptSizedToolTurn(t, n, "lookup", map[string]any{"q": "y"}, 10, tc.second, 0)
				default:
					return promptSizedToolTurn(t, n, "submit_result", submitInput("done"), 10, 0, 0)
				}
			},
				claudeexecutor.WithMaxToolCallsBeforeFinalize[errCapRequest, errCapResponse](1),
				claudeexecutor.WithPromptTokensBeforeFinalize[errCapRequest, errCapResponse](500))
			if len(bodies) != 3 {
				t.Fatalf("requests: got = %d, want = 3", len(bodies))
			}
			for i := range 2 {
				body := bodies[i+1]
				if got := strings.Count(body, promptBudgetNudge); got != tc.wantPrompt[i] {
					t.Errorf("request %d prompt-budget instructions: got = %d, want = %d", i+2, got, tc.wantPrompt[i])
				}
				if got := strings.Count(body, toolCapNudge); got != tc.wantToolCap[i] {
					t.Errorf("request %d tool-call instructions: got = %d, want = %d", i+2, got, tc.wantToolCap[i])
				}
				if got := forcesSubmit(t, body); got != tc.wantForced[i] {
					t.Errorf("request %d forces submit_result: got = %v, want = %v", i+2, got, tc.wantForced[i])
				}
			}
		})
	}
}

// TestPromptBudgetNeedsASubmitTool pins that without a terminal tool there is
// nothing to steer toward, so a prompt over budget changes no request.
func TestPromptBudgetNeedsASubmitTool(t *testing.T) {
	client, requests := newExecTestClient(t, func(n int) []string {
		if n == 1 {
			return promptSizedToolTurn(t, n, "lookup", map[string]any{"q": "x"}, 10, 1000, 0)
		}
		return answerTurn
	})
	prompt, err := promptbuilder.NewPrompt("Search the repository.")
	if err != nil {
		t.Fatal(err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt,
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithPromptTokensBeforeFinalize[errCapRequest, errCapResponse](500),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{
		"lookup": {
			Definition: anthropic.ToolParam{Name: "lookup", InputSchema: anthropic.ToolInputSchemaParam{Type: "object"}},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"result": "evidence"}
			},
		},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	second := string(requests()[1])
	if strings.Contains(second, promptBudgetNudge) || forcesATool(t, second) {
		t.Errorf("second request: got a finalize instruction or forced tool, want neither without a submit tool")
	}
}

// TestToolCallCapNudgesOnce pins the tool-call cap for its existing callers:
// after a rejected submission it neither repeats its instruction nor forces
// submit again, so the model can call the tools the rejection asks for.
func TestToolCallCapNudgesOnce(t *testing.T) {
	const toolCapNudge = "You have gathered enough evidence. Call the submit_result tool now to return your result based on what you have found so far."
	bodies := promptBudgetRun(t, func(n int) []string {
		switch n {
		case 1:
			return promptSizedToolTurn(t, n, "lookup", map[string]any{"q": "x"}, 10, 0, 0)
		case 2:
			return promptSizedToolTurn(t, n, "submit_result", submitInput("wrong"), 10, 0, 0)
		case 3:
			return promptSizedToolTurn(t, n, "lookup", map[string]any{"q": "y"}, 10, 0, 0)
		default:
			return promptSizedToolTurn(t, n, "submit_result", submitInput("done"), 10, 0, 0)
		}
	}, claudeexecutor.WithMaxToolCallsBeforeFinalize[errCapRequest, errCapResponse](1))

	if len(bodies) != 4 {
		t.Fatalf("requests: got = %d, want = 4", len(bodies))
	}
	if got := strings.Count(bodies[3], toolCapNudge); got != 1 {
		t.Errorf("tool-call instructions in the final request: got = %d, want = 1", got)
	}
	for i, want := range []bool{false, true, false, false} {
		if got := forcesSubmit(t, bodies[i]); got != want {
			t.Errorf("request %d forces submit_result: got = %v, want = %v", i+1, got, want)
		}
	}
}

// TestPromptBudgetWithThinkingSendsTheInstructionUnforced pins that a run with
// thinking on still gets the instruction but no forced tool_choice, which the
// API would refuse with a 400 just as the prompt nears the window.
func TestPromptBudgetWithThinkingSendsTheInstructionUnforced(t *testing.T) {
	bodies := promptBudgetRun(t, func(n int) []string {
		if n == 1 {
			return promptSizedToolTurn(t, n, "lookup", map[string]any{"q": "x"}, 10, 1000, 0)
		}
		return promptSizedToolTurn(t, n, "submit_result", submitInput("done"), 10, 0, 0)
	},
		claudeexecutor.WithThinking[errCapRequest, errCapResponse](2048),
		claudeexecutor.WithPromptTokensBeforeFinalize[errCapRequest, errCapResponse](500))
	if len(bodies) != 2 {
		t.Fatalf("requests: got = %d, want = 2", len(bodies))
	}
	if got := strings.Count(bodies[1], promptBudgetNudge); got != 1 {
		t.Errorf("prompt-budget instructions: got = %d, want = 1", got)
	}
	if forcesSubmit(t, bodies[1]) {
		t.Error("second request forces submit_result with thinking on, want automatic tool choice")
	}
}

// TestToolCallCapWithThinkingSendsTheInstructionUnforced pins the same rule
// for the tool-call cap, which shares the nudge: with thinking on, its
// instruction goes out and nothing is forced.
func TestToolCallCapWithThinkingSendsTheInstructionUnforced(t *testing.T) {
	bodies := promptBudgetRun(t, func(n int) []string {
		if n == 1 {
			return promptSizedToolTurn(t, n, "lookup", map[string]any{"q": "x"}, 10, 0, 0)
		}
		return promptSizedToolTurn(t, n, "submit_result", submitInput("done"), 10, 0, 0)
	},
		claudeexecutor.WithThinking[errCapRequest, errCapResponse](2048),
		claudeexecutor.WithMaxToolCallsBeforeFinalize[errCapRequest, errCapResponse](1))
	if len(bodies) != 2 {
		t.Fatalf("requests: got = %d, want = 2", len(bodies))
	}
	if !strings.Contains(bodies[1], "You have gathered enough evidence") {
		t.Error("second request lacks the tool-call instruction")
	}
	if forcesSubmit(t, bodies[1]) {
		t.Error("second request forces submit_result with thinking on, want automatic tool choice")
	}
}
