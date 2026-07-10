/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"encoding/json"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// hasCacheControl reports whether a value marshals with a cache_control
// breakpoint. The SDK tags CacheControl omitzero, so a zero value is omitted
// from the JSON and a placed breakpoint appears as a cache_control object.
func hasCacheControl(t *testing.T, v any) bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal for cache_control check: %v", err)
	}
	return strings.Contains(string(b), `"cache_control"`)
}

// newTestExecutor builds an executor with the given options for white-box
// inspection of assembleParams. The client is never used because assembleParams
// makes no network calls.
func newTestExecutor(t *testing.T, opts ...Option[*testBindable, *testResponse]) *executor[*testBindable, *testResponse] {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}
	exec, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt, opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return exec.(*executor[*testBindable, *testResponse])
}

// marshalParams serializes assembled params to JSON so two assemblies can be
// compared byte-for-byte. The Anthropic params marshal deterministically
// (struct field order is fixed; tool defs are pre-sorted by assembleParams).
func marshalParams(t *testing.T, p anthropic.MessageNewParams) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return b
}

// systemInstructions returns a non-nil system prompt for tests that exercise
// the system-block branch of assembleParams.
func systemInstructions(t *testing.T) *promptbuilder.Prompt {
	t.Helper()
	sys, err := promptbuilder.NewPrompt("system instructions body")
	if err != nil {
		t.Fatalf("NewPrompt(system): %v", err)
	}
	return sys
}

// twoTools returns a tool map with two named tools so assembleParams sorts and
// places a tool-definition breakpoint.
func twoTools() map[string]claudetool.Metadata[*testResponse] {
	return map[string]claudetool.Metadata[*testResponse]{
		"alpha": {Definition: anthropic.ToolParam{Name: "alpha"}},
		"zebra": {Definition: anthropic.ToolParam{Name: "zebra"}},
	}
}

// TestAssembleParamsTransparentWhenOptionsOff is the load-bearing transparency
// check: with the new opt-in levers unset, the assembled request must be
// byte-identical to an executor that never knew about them. We compare the
// default executor (no new options) against one constructed with the new
// options explicitly defaulted (CacheFirstUserBlock not called,
// MaxToolCallsBeforeFinalize=0), across the realistic shapes a caller hits:
// with/without a system prompt and with/without tools.
func TestAssembleParamsTransparentWhenOptionsOff(t *testing.T) {
	t.Parallel()

	const prompt = "analyze this large payload of evidence"

	shapes := []struct {
		name       string
		withSystem bool
		tools      map[string]claudetool.Metadata[*testResponse]
	}{
		{name: "no system, no tools", withSystem: false, tools: nil},
		{name: "system, no tools", withSystem: true, tools: nil},
		{name: "system and tools", withSystem: true, tools: twoTools()},
		{name: "no system, tools", withSystem: false, tools: twoTools()},
	}

	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()

			baseOpts := func() []Option[*testBindable, *testResponse] {
				var o []Option[*testBindable, *testResponse]
				if sh.withSystem {
					o = append(o, WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)))
				}
				return o
			}

			// Baseline: the executor as it exists for callers who never set the
			// new options.
			base := newTestExecutor(t, baseOpts()...)
			baseParams, _, err := base.assembleParams(prompt, "", sh.tools)
			if err != nil {
				t.Fatalf("baseline assembleParams: %v", err)
			}

			// Same executor, but with the new options explicitly at their
			// default (off / zero). This must produce identical bytes.
			withDefaults := newTestExecutor(t, append(baseOpts(),
				WithMaxToolCallsBeforeFinalize[*testBindable, *testResponse](0),
			)...)
			defaultParams, _, err := withDefaults.assembleParams(prompt, "", sh.tools)
			if err != nil {
				t.Fatalf("defaults assembleParams: %v", err)
			}

			if got, want := marshalParams(t, defaultParams), marshalParams(t, baseParams); string(got) != string(want) {
				t.Errorf("request bytes diverged with new options at default:\n got = %s\nwant = %s", got, want)
			}
		})
	}
}

// TestCacheFirstUserBlockOnlyWhenEnabled asserts the first-user-block
// breakpoint appears exactly when WithCacheFirstUserBlock is set and is absent
// otherwise. We inspect the assembled CacheControl on the first user content
// block directly.
func TestCacheFirstUserBlockOnlyWhenEnabled(t *testing.T) {
	t.Parallel()

	firstUserBlockCached := func(p anthropic.MessageNewParams) bool {
		if len(p.Messages) == 0 || len(p.Messages[0].Content) == 0 {
			return false
		}
		tb := p.Messages[0].Content[0].OfText
		return tb != nil && hasCacheControl(t, tb)
	}

	// Off (default): no breakpoint on the first user block.
	off := newTestExecutor(t, WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)))
	offParams, _, err := off.assembleParams("payload", "", twoTools())
	if err != nil {
		t.Fatalf("off assembleParams: %v", err)
	}
	if firstUserBlockCached(offParams) {
		t.Error("first user block has cache_control with option off; want none")
	}

	// On: breakpoint present.
	on := newTestExecutor(t,
		WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)),
		WithCacheFirstUserBlock[*testBindable, *testResponse](),
	)
	onParams, _, err := on.assembleParams("payload", "", twoTools())
	if err != nil {
		t.Fatalf("on assembleParams: %v", err)
	}
	if !firstUserBlockCached(onParams) {
		t.Error("first user block missing cache_control with option on; want present")
	}
}

// TestCacheFirstUserBlockNoOpWithoutCacheControl confirms the first-user-block
// breakpoint is suppressed when prompt caching is disabled entirely — the
// option composes under WithoutCacheControl without producing a stray
// breakpoint.
func TestCacheFirstUserBlockNoOpWithoutCacheControl(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithoutCacheControl[*testBindable, *testResponse](),
		WithCacheFirstUserBlock[*testBindable, *testResponse](),
	)
	params, _, err := exec.assembleParams("payload", "", twoTools())
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}
	if tb := params.Messages[0].Content[0].OfText; tb != nil && hasCacheControl(t, tb) {
		t.Error("first user block cached despite WithoutCacheControl; want no breakpoint")
	}
}

// TestCacheBreakpointCountWithinLimit verifies the executor never emits more
// than maxCacheBreakpoints markers. With caching on, a system prompt, tools,
// and the first-user-block option all enabled, the assembly uses three of the
// four available slots and stays within the limit.
func TestCacheBreakpointCountWithinLimit(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)),
		WithCacheFirstUserBlock[*testBindable, *testResponse](),
	)
	params, _, err := exec.assembleParams("payload", "", twoTools())
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}

	count := countCacheBreakpoints(t, params)
	if count > maxCacheBreakpoints {
		t.Errorf("breakpoint count = %d, exceeds limit %d", count, maxCacheBreakpoints)
	}
	// Tools + system + first user block = exactly three breakpoints.
	if count != 3 {
		t.Errorf("breakpoint count = %d, want 3 (tools, system, first user block)", count)
	}
}

// countCacheBreakpoints counts every cache_control marker in an assembled
// request: the last tool definition, the system block, and the first user
// content block.
func countCacheBreakpoints(t *testing.T, p anthropic.MessageNewParams) int {
	t.Helper()
	n := 0
	for i := range p.Tools {
		if p.Tools[i].OfTool != nil && hasCacheControl(t, p.Tools[i].OfTool) {
			n++
		}
	}
	for i := range p.System {
		if hasCacheControl(t, p.System[i]) {
			n++
		}
	}
	for _, m := range p.Messages {
		for _, c := range m.Content {
			if c.OfText != nil && hasCacheControl(t, c.OfText) {
				n++
			}
		}
	}
	return n
}

// TestShouldNudgeFinalize is the decision table for the early-finalize nudge:
// off by default (threshold 0), fires only at or past the cap, never before,
// at most once, and only when a terminal tool is configured.
func TestShouldNudgeFinalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		threshold     int
		liveToolCalls int
		alreadyNudged bool
		submitTool    string
		want          bool
	}{
		{name: "off by default (threshold zero)", threshold: 0, liveToolCalls: 100, submitTool: "submit_result", want: false},
		{name: "negative threshold never fires", threshold: -1, liveToolCalls: 100, submitTool: "submit_result", want: false},
		{name: "below cap does not fire", threshold: 3, liveToolCalls: 2, submitTool: "submit_result", want: false},
		{name: "at cap fires", threshold: 3, liveToolCalls: 3, submitTool: "submit_result", want: true},
		{name: "past cap fires", threshold: 3, liveToolCalls: 5, submitTool: "submit_result", want: true},
		{name: "already nudged does not refire", threshold: 3, liveToolCalls: 5, alreadyNudged: true, submitTool: "submit_result", want: false},
		{name: "no submit tool never fires", threshold: 3, liveToolCalls: 5, submitTool: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shouldNudgeFinalize(tt.threshold, tt.liveToolCalls, tt.alreadyNudged, tt.submitTool)
			if got != tt.want {
				t.Errorf("shouldNudgeFinalize(%d, %d, %v, %q) = %v, want %v",
					tt.threshold, tt.liveToolCalls, tt.alreadyNudged, tt.submitTool, got, tt.want)
			}
		})
	}
}

// TestWithMaxToolCallsBeforeFinalizeValidation checks the option rejects
// negative values and accepts zero (the disabled default) and positive caps.
func TestWithMaxToolCallsBeforeFinalizeValidation(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("p")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	tests := []struct {
		name    string
		n       int
		wantErr bool
	}{
		{name: "zero (disabled)", n: 0, wantErr: false},
		{name: "positive", n: 4, wantErr: false},
		{name: "negative", n: -1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exec, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt,
				WithMaxToolCallsBeforeFinalize[*testBindable, *testResponse](tt.n))
			if (err != nil) != tt.wantErr {
				t.Fatalf("WithMaxToolCallsBeforeFinalize(%d) err = %v, wantErr %v", tt.n, err, tt.wantErr)
			}
			if err == nil {
				e := exec.(*executor[*testBindable, *testResponse])
				if e.maxToolCallsBeforeFinalize != tt.n {
					t.Errorf("maxToolCallsBeforeFinalize = %d, want %d", e.maxToolCallsBeforeFinalize, tt.n)
				}
			}
		})
	}
}
