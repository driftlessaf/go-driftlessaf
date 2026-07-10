/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// userTextMessage builds a user message with a single text block, the shape of
// the initial prompt and of redirect/nudge messages.
func userTextMessage(text string) anthropic.MessageParam {
	return anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(text)},
	}
}

// toolResultMessage builds a user message carrying n tool_result blocks, the
// shape appended after each turn's tool dispatch.
func toolResultMessage(n int) anthropic.MessageParam {
	content := make([]anthropic.ContentBlockParamUnion, 0, n)
	for i := range n {
		content = append(content, anthropic.ContentBlockParamUnion{
			OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: string(rune('a' + i)),
			},
		})
	}
	return anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleUser,
		Content: content,
	}
}

// assistantToolUseMessage builds an assistant message with a tool_use block,
// the shape appended when the model calls tools.
func assistantToolUseMessage(id string) anthropic.MessageParam {
	return anthropic.MessageParam{
		Role: anthropic.MessageParamRoleAssistant,
		Content: []anthropic.ContentBlockParamUnion{{
			OfToolUse: &anthropic.ToolUseBlockParam{ID: id, Name: "probe"},
		}},
	}
}

// countMessageBreakpoints counts cache_control markers across every cacheable
// block variant in the conversation. Unlike countCacheBreakpoints it covers
// tool_use and tool_result blocks, which is where the tail markers land.
func countMessageBreakpoints(t *testing.T, messages []anthropic.MessageParam) int {
	t.Helper()
	n := 0
	for _, m := range messages {
		for i := range m.Content {
			c := &m.Content[i]
			var v any
			switch {
			case c.OfText != nil:
				v = c.OfText
			case c.OfToolResult != nil:
				v = c.OfToolResult
			case c.OfToolUse != nil:
				v = c.OfToolUse
			default:
				continue
			}
			if hasCacheControl(t, v) {
				n++
			}
		}
	}
	return n
}

// TestTailBreakpointsAdvance verifies the basic contract: the first advance
// marks the last block of the newest message, and advancing again without new
// messages is a no-op (no duplicate positions, no extra markers).
func TestTailBreakpointsAdvance(t *testing.T) {
	t.Parallel()

	messages := []anthropic.MessageParam{userTextMessage("initial prompt")}
	tail := &tailBreakpoints{limit: tailBreakpointLimit}

	tail.advance(messages)
	if got, want := countMessageBreakpoints(t, messages), 1; got != want {
		t.Fatalf("breakpoints after first advance: got = %d, want = %d", got, want)
	}
	if got, want := len(tail.positions), 1; got != want {
		t.Fatalf("tracked positions: got = %d, want = %d", got, want)
	}
	if got, want := tail.positions[0], (tailPosition{message: 0, block: 0}); got != want {
		t.Errorf("tail position: got = %v, want = %v", got, want)
	}

	// No new messages: advancing again must not add markers or positions.
	tail.advance(messages)
	if got, want := countMessageBreakpoints(t, messages), 1; got != want {
		t.Errorf("breakpoints after redundant advance: got = %d, want = %d", got, want)
	}
	if got, want := len(tail.positions), 1; got != want {
		t.Errorf("tracked positions after redundant advance: got = %d, want = %d", got, want)
	}
}

// TestTailBreakpointsRotation grows the conversation across three turns and
// verifies the tracker keeps the two newest markers and clears the oldest, so
// the tail never consumes more than tailBreakpointLimit of the API's
// breakpoint budget.
func TestTailBreakpointsRotation(t *testing.T) {
	t.Parallel()

	messages := []anthropic.MessageParam{userTextMessage("initial prompt")}
	tail := &tailBreakpoints{limit: tailBreakpointLimit}
	tail.advance(messages)

	// Turn 1: assistant tool call + tool results appended.
	messages = append(messages, assistantToolUseMessage("t1"), toolResultMessage(2))
	tail.advance(messages)

	// Turn 2: another round appended — the initial-prompt marker must rotate out.
	messages = append(messages, assistantToolUseMessage("t2"), toolResultMessage(1))
	tail.advance(messages)

	if got, want := countMessageBreakpoints(t, messages), tailBreakpointLimit; got != want {
		t.Errorf("breakpoints after rotation: got = %d, want = %d", got, want)
	}
	if got, want := len(tail.positions), tailBreakpointLimit; got != want {
		t.Fatalf("tracked positions after rotation: got = %d, want = %d", got, want)
	}

	// The oldest marker (the initial prompt) must have been cleared.
	if hasCacheControl(t, messages[0].Content[0].OfText) {
		t.Error("initial prompt block still marked after rotation; want cleared")
	}
	// The two newest tool-result messages carry the markers, on their last blocks.
	wantPositions := []tailPosition{
		{message: 2, block: 1},
		{message: 4, block: 0},
	}
	for i, want := range wantPositions {
		if got := tail.positions[i]; got != want {
			t.Errorf("positions[%d]: got = %v, want = %v", i, got, want)
		}
	}
}

// TestTailBreakpointsSkipsNonCacheable verifies the backward walk within the
// newest message: block types without a CacheControl field are skipped, and a
// message with no cacheable block at all leaves the tracker untouched.
func TestTailBreakpointsSkipsNonCacheable(t *testing.T) {
	t.Parallel()

	t.Run("walks past a trailing non-cacheable block", func(t *testing.T) {
		t.Parallel()
		messages := []anthropic.MessageParam{{
			Role: anthropic.MessageParamRoleAssistant,
			Content: []anthropic.ContentBlockParamUnion{
				{OfToolUse: &anthropic.ToolUseBlockParam{ID: "x", Name: "probe"}},
				{OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{Data: "opaque"}},
			},
		}}
		tail := &tailBreakpoints{limit: tailBreakpointLimit}
		tail.advance(messages)
		if got, want := len(tail.positions), 1; got != want {
			t.Fatalf("tracked positions: got = %d, want = %d", got, want)
		}
		if got, want := tail.positions[0], (tailPosition{message: 0, block: 0}); got != want {
			t.Errorf("tail position: got = %v, want = %v", got, want)
		}
		if !hasCacheControl(t, messages[0].Content[0].OfToolUse) {
			t.Error("tool_use block not marked; want marker on the last cacheable block")
		}
	})

	t.Run("no cacheable block is a no-op", func(t *testing.T) {
		t.Parallel()
		messages := []anthropic.MessageParam{{
			Role: anthropic.MessageParamRoleAssistant,
			Content: []anthropic.ContentBlockParamUnion{
				{OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{Data: "opaque"}},
			},
		}}
		tail := &tailBreakpoints{limit: tailBreakpointLimit}
		tail.advance(messages)
		if got, want := len(tail.positions), 0; got != want {
			t.Errorf("tracked positions: got = %d, want = %d", got, want)
		}
	})

	t.Run("empty conversation is a no-op", func(t *testing.T) {
		t.Parallel()
		tail := &tailBreakpoints{limit: tailBreakpointLimit}
		tail.advance(nil)
		if got, want := len(tail.positions), 0; got != want {
			t.Errorf("tracked positions: got = %d, want = %d", got, want)
		}
	})
}

// TestTailBreakpointsSeededFromFirstUserBlock mirrors how Execute seeds the
// tracker when WithCacheFirstUserBlock placed a marker during assembly: the
// seeded position must suppress a duplicate mark on the first turn and rotate
// out (clearing the option's marker) once the conversation outgrows it.
func TestTailBreakpointsSeededFromFirstUserBlock(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)),
		WithCacheFirstUserBlock[*testBindable, *testResponse](),
	)
	params, _, err := exec.assembleParams("large payload", "", twoTools())
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}

	tail := newTailBreakpoints(params)
	tail.positions = append(tail.positions, tailPosition{message: 0, block: 0})

	// Turn 0: the tail is the block the option already marked — no duplicate.
	tail.advance(params.Messages)
	if got, want := len(tail.positions), 1; got != want {
		t.Fatalf("tracked positions after turn 0: got = %d, want = %d", got, want)
	}

	// Two more turns: the seeded marker rotates out and is cleared.
	params.Messages = append(params.Messages, assistantToolUseMessage("t1"), toolResultMessage(1))
	tail.advance(params.Messages)
	params.Messages = append(params.Messages, assistantToolUseMessage("t2"), toolResultMessage(1))
	tail.advance(params.Messages)

	if hasCacheControl(t, params.Messages[0].Content[0].OfText) {
		t.Error("first-user-block marker still present after rotation; want cleared")
	}
	if got, want := countMessageBreakpoints(t, params.Messages), tailBreakpointLimit; got != want {
		t.Errorf("message breakpoints: got = %d, want = %d", got, want)
	}
}

// TestTailBreakpointsTotalWithinLimit exercises the worst-case budget: system
// prompt, tools, the first-user-block option, and a multi-turn conversation
// with tail rotation. The total marker count across tools, system, and
// messages must never exceed the API's limit of four.
func TestTailBreakpointsTotalWithinLimit(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)),
		WithCacheFirstUserBlock[*testBindable, *testResponse](),
	)
	params, _, err := exec.assembleParams("payload", "", twoTools())
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}

	// Seed as Execute does when the first-user-block option is on.
	tail := newTailBreakpoints(params)
	tail.positions = append(tail.positions, tailPosition{message: 0, block: 0})

	total := func() int {
		n := 0
		for i := range params.Tools {
			if params.Tools[i].OfTool != nil && hasCacheControl(t, params.Tools[i].OfTool) {
				n++
			}
		}
		for i := range params.System {
			if hasCacheControl(t, params.System[i]) {
				n++
			}
		}
		return n + countMessageBreakpoints(t, params.Messages)
	}

	// Simulate five turns of tool calls and results, advancing before each
	// request as executeTurn does.
	for turn := range 5 {
		tail.advance(params.Messages)
		if got := total(); got > maxCacheBreakpoints {
			t.Fatalf("turn %d: total breakpoints = %d, exceeds limit %d", turn, got, maxCacheBreakpoints)
		}
		params.Messages = append(params.Messages,
			assistantToolUseMessage("t"), toolResultMessage(2))
	}

	// Steady state: tools + system + two tail markers = exactly four.
	tail.advance(params.Messages)
	if got, want := total(), maxCacheBreakpoints; got != want {
		t.Errorf("steady-state breakpoints: got = %d, want = %d", got, want)
	}
}

// callerMarkedTools returns a tool map where the caller placed its own
// cache_control markers on n of the tool definitions, consuming API budget
// before the executor places anything.
func callerMarkedTools(n int) map[string]claudetool.Metadata[*testResponse] {
	tools := map[string]claudetool.Metadata[*testResponse]{
		"alpha": {Definition: anthropic.ToolParam{Name: "alpha"}},
		"bravo": {Definition: anthropic.ToolParam{Name: "bravo"}},
		"zebra": {Definition: anthropic.ToolParam{Name: "zebra"}},
	}
	marked := 0
	for _, name := range []string{"alpha", "bravo", "zebra"} {
		if marked == n {
			break
		}
		meta := tools[name]
		meta.Definition.CacheControl = anthropic.NewCacheControlEphemeralParam()
		tools[name] = meta
		marked++
	}
	return tools
}

// TestTailBudgetRespectsCallerMarkers verifies the runtime guard on the API's
// breakpoint limit: caller-supplied markers on tool definitions shrink the
// tail budget (and, at the extreme, the executor's own static placements),
// so the request total never exceeds maxCacheBreakpoints.
func TestTailBudgetRespectsCallerMarkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		callerMarkers int
		wantTailLimit int
	}{
		{name: "no caller markers keeps full tail budget", callerMarkers: 0, wantTailLimit: tailBreakpointLimit},
		{name: "one caller marker leaves one tail slot", callerMarkers: 1, wantTailLimit: 1},
		{name: "two caller markers leave no tail slots", callerMarkers: 2, wantTailLimit: 0},
		{name: "three caller markers disable tail caching", callerMarkers: 3, wantTailLimit: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			exec := newTestExecutor(t, WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)))
			params, _, err := exec.assembleParams("payload", "", callerMarkedTools(tt.callerMarkers))
			if err != nil {
				t.Fatalf("assembleParams: %v", err)
			}

			tail := newTailBreakpoints(params)
			if got, want := tail.limit, tt.wantTailLimit; got != want {
				t.Errorf("tail limit: got = %d, want = %d", got, want)
			}

			total := func() int {
				n := staticBreakpointCount(params)
				return n + countMessageBreakpoints(t, params.Messages)
			}

			// Simulate turns; the total must never exceed the API limit.
			for turn := range 4 {
				tail.advance(params.Messages)
				if got := total(); got > maxCacheBreakpoints {
					t.Fatalf("turn %d: total breakpoints = %d, exceeds limit %d", turn, got, maxCacheBreakpoints)
				}
				params.Messages = append(params.Messages,
					assistantToolUseMessage("t"), toolResultMessage(1))
			}

			// With zero budget, no message may carry a marker.
			if tt.wantTailLimit == 0 {
				if got := countMessageBreakpoints(t, params.Messages); got != 0 {
					t.Errorf("message breakpoints with zero budget: got = %d, want = 0", got)
				}
			}
		})
	}
}

// TestSeedFirstUserTail drives the seeding decision through real assembled
// params, covering the guards that Execute relies on: the option flags, and —
// crucially — that seeding only happens when the first-user marker was
// actually placed (assembleParams suppresses it when caller-supplied markers
// exhausted the breakpoint budget).
func TestSeedFirstUserTail(t *testing.T) {
	t.Parallel()

	assemble := func(t *testing.T, exec *executor[*testBindable, *testResponse], tools map[string]claudetool.Metadata[*testResponse]) anthropic.MessageNewParams {
		t.Helper()
		params, _, err := exec.assembleParams("payload", "", tools)
		if err != nil {
			t.Fatalf("assembleParams: %v", err)
		}
		return params
	}

	t.Run("seeds when option on and marker placed", func(t *testing.T) {
		t.Parallel()
		exec := newTestExecutor(t,
			WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)),
			WithCacheFirstUserBlock[*testBindable, *testResponse](),
		)
		params := assemble(t, exec, twoTools())
		got := seedFirstUserTail(exec.cacheControl, exec.cacheFirstUserBlock, params)
		if want := []tailPosition{{message: 0, block: 0}}; len(got) != 1 || got[0] != want[0] {
			t.Errorf("seed positions: got = %v, want = %v", got, want)
		}
	})

	t.Run("no seed when option off", func(t *testing.T) {
		t.Parallel()
		exec := newTestExecutor(t, WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)))
		params := assemble(t, exec, twoTools())
		if got := seedFirstUserTail(exec.cacheControl, exec.cacheFirstUserBlock, params); got != nil {
			t.Errorf("seed positions: got = %v, want = nil", got)
		}
	})

	t.Run("no seed when caching disabled", func(t *testing.T) {
		t.Parallel()
		exec := newTestExecutor(t,
			WithoutCacheControl[*testBindable, *testResponse](),
			WithCacheFirstUserBlock[*testBindable, *testResponse](),
		)
		params := assemble(t, exec, twoTools())
		if got := seedFirstUserTail(exec.cacheControl, exec.cacheFirstUserBlock, params); got != nil {
			t.Errorf("seed positions: got = %v, want = nil", got)
		}
	})

	t.Run("no seed when budget suppressed the marker", func(t *testing.T) {
		t.Parallel()
		// Three caller markers + the system marker consume the full budget, so
		// assembleParams never places the first-user marker: seeding must not
		// invent a position for a marker that is not there.
		exec := newTestExecutor(t,
			WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)),
			WithCacheFirstUserBlock[*testBindable, *testResponse](),
		)
		params := assemble(t, exec, callerMarkedTools(3))
		if got := seedFirstUserTail(exec.cacheControl, exec.cacheFirstUserBlock, params); got != nil {
			t.Errorf("seed positions: got = %v, want = nil", got)
		}
	})
}
