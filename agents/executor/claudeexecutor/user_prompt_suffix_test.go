/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/anthropics/anthropic-sdk-go"
)

// suffixPrompt returns a fully bound prompt for tests that exercise the
// user-prompt-suffix split.
func suffixPrompt(t *testing.T) *promptbuilder.Prompt {
	t.Helper()
	p, err := promptbuilder.NewPrompt("lens suffix body")
	if err != nil {
		t.Fatalf("NewPrompt(suffix): %v", err)
	}
	return p
}

// TestUserPromptSuffixSplitsFirstUserMessage verifies the block layout the
// cross-execution cache sharing depends on: with a suffix configured, the
// initial user message carries two text blocks — the rendered prompt, then
// the suffix — with the cache breakpoint on block 0 (ending the shareable
// prefix) and none on block 1 (the varying suffix).
func TestUserPromptSuffixSplitsFirstUserMessage(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)),
		WithUserPromptSuffix[*testBindable, *testResponse](suffixPrompt(t)),
	)
	params, _, err := exec.assembleParams("changeset payload", "lens suffix body", twoTools())
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}

	if got, want := len(params.Messages), 1; got != want {
		t.Fatalf("initial messages: got = %d, want = %d", got, want)
	}
	content := params.Messages[0].Content
	if got, want := len(content), 2; got != want {
		t.Fatalf("first user message blocks: got = %d, want = %d", got, want)
	}

	blocks := []struct {
		name       string
		wantText   string
		wantMarker bool
	}{
		{name: "payload block", wantText: "changeset payload", wantMarker: true},
		{name: "suffix block", wantText: "lens suffix body", wantMarker: false},
	}
	for i, b := range blocks {
		tb := content[i].OfText
		if tb == nil {
			t.Fatalf("%s: not a text block", b.name)
		}
		if got, want := tb.Text, b.wantText; got != want {
			t.Errorf("%s text: got = %q, want = %q", b.name, got, want)
		}
		if got, want := hasCacheControl(t, tb), b.wantMarker; got != want {
			t.Errorf("%s cache marker: got = %v, want = %v", b.name, got, want)
		}
	}
}

// TestUserPromptSuffixAbsentIsTransparent is the transparency check for the
// suffix plumbing: an executor that never sets the option must assemble a
// request byte-identical to one built from the same configuration, with the
// initial user message keeping its single-block shape.
func TestUserPromptSuffixAbsentIsTransparent(t *testing.T) {
	t.Parallel()

	assemble := func(t *testing.T) anthropic.MessageNewParams {
		t.Helper()
		exec := newTestExecutor(t, WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)))
		params, _, err := exec.assembleParams("payload", "", twoTools())
		if err != nil {
			t.Fatalf("assembleParams: %v", err)
		}
		return params
	}

	baseline := assemble(t)
	repeat := assemble(t)
	if got, want := marshalParams(t, repeat), marshalParams(t, baseline); string(got) != string(want) {
		t.Errorf("request bytes diverged without the suffix option:\n got = %s\nwant = %s", got, want)
	}

	if got, want := len(baseline.Messages[0].Content), 1; got != want {
		t.Errorf("first user message blocks without suffix: got = %d, want = %d", got, want)
	}
	if tb := baseline.Messages[0].Content[0].OfText; tb == nil || tb.Text != "payload" {
		t.Errorf("first user block: got = %+v, want text block %q", tb, "payload")
	}
}

// TestUserPromptSuffixImpliesFirstUserBlockMarker verifies the option's side
// effect: setting a suffix turns on the first-user-block cache marker, since
// the leading block only becomes a shareable prefix with a breakpoint on it.
func TestUserPromptSuffixImpliesFirstUserBlockMarker(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t, WithUserPromptSuffix[*testBindable, *testResponse](suffixPrompt(t)))
	if !exec.cacheFirstUserBlock {
		t.Error("cacheFirstUserBlock: got = false, want = true (implied by WithUserPromptSuffix)")
	}
}

// TestUserPromptSuffixNilReturnsError verifies the option rejects a nil
// suffix at construction, matching the other prompt-carrying options.
func TestUserPromptSuffixNilReturnsError(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("p")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	if _, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt,
		WithUserPromptSuffix[*testBindable, *testResponse](nil)); err == nil {
		t.Error("New(WithUserPromptSuffix(nil)): got = nil, want error")
	}
}

// TestUserPromptSuffixBudgetWithinLimit exercises the worst-case breakpoint
// budget with the suffix configured, mirroring TestTailBreakpointsTotalWithinLimit:
// tools (1) + system (1) + the seeded payload marker {0,0} + the turn-0 tail
// advance onto the suffix block {0,1} come to exactly four, and rotation keeps
// the total within the API limit on every subsequent turn.
func TestUserPromptSuffixBudgetWithinLimit(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)),
		WithUserPromptSuffix[*testBindable, *testResponse](suffixPrompt(t)),
	)
	params, _, err := exec.assembleParams("payload", "lens suffix body", twoTools())
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}

	// Seed as Execute does: the suffix option implies the first-user marker.
	tail := newTailBreakpoints(params)
	tail.positions = seedFirstUserTail(exec.cacheControl, exec.cacheFirstUserBlock, params)
	if got, want := len(tail.positions), 1; got != want {
		t.Fatalf("seeded tail positions: got = %d, want = %d", got, want)
	}

	total := func() int {
		return staticBreakpointCount(params) + countMessageBreakpoints(t, params.Messages)
	}

	// Turn 0: the tail advances onto the suffix block {0,1}, joining the
	// seeded payload marker {0,0} within the tail budget.
	tail.advance(params.Messages)
	wantPositions := []tailPosition{{message: 0, block: 0}, {message: 0, block: 1}}
	if got, want := len(tail.positions), len(wantPositions); got != want {
		t.Fatalf("tail positions after turn 0: got = %d, want = %d", got, want)
	}
	for i, want := range wantPositions {
		if got := tail.positions[i]; got != want {
			t.Errorf("positions[%d]: got = %v, want = %v", i, got, want)
		}
	}
	if got, want := total(), maxCacheBreakpoints; got != want {
		t.Errorf("turn 0 breakpoints: got = %d, want = %d", got, want)
	}

	// Simulate five turns of tool calls and results, advancing before each
	// request as executeTurn does; the total must never exceed the API limit.
	for turn := range 5 {
		params.Messages = append(params.Messages,
			assistantToolUseMessage("t"), toolResultMessage(2))
		tail.advance(params.Messages)
		if got := total(); got > maxCacheBreakpoints {
			t.Fatalf("turn %d: total breakpoints = %d, exceeds limit %d", turn+1, got, maxCacheBreakpoints)
		}
	}

	// Steady state: tools + system + two tail markers = exactly four.
	if got, want := total(), maxCacheBreakpoints; got != want {
		t.Errorf("steady-state breakpoints: got = %d, want = %d", got, want)
	}
}

// TestTracePrompt pins the trace-recording contract: the recorded prompt is
// the payload plus the suffix (joined like the single-prompt backends fold
// them), so traces of multi-pass agents show which pass ran; without a suffix
// the prompt passes through unchanged.
func TestTracePrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prompt string
		suffix string
		want   string
	}{
		{name: "no suffix passes through", prompt: "payload", suffix: "", want: "payload"},
		{name: "suffix appended with blank line", prompt: "payload", suffix: "lens", want: "payload\n\nlens"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tracePrompt(tt.prompt, tt.suffix); got != tt.want {
				t.Errorf("tracePrompt(%q, %q) = %q, want %q", tt.prompt, tt.suffix, got, tt.want)
			}
		})
	}
}
