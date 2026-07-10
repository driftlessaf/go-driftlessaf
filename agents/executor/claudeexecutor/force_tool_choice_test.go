/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"context"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// newTestExecutorErr builds an executor and returns the construction error
// instead of failing the test, so option-validation failures can be asserted.
func newTestExecutorErr(t *testing.T, opts ...Option[*testBindable, *testResponse]) (Interface[*testBindable, *testResponse], error) {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}
	return New[*testBindable, *testResponse](anthropic.Client{}, prompt, opts...)
}

// submitToolTestName is the terminal submit tool name used across the
// force-tool-choice tests; it stands in for the production emit_verdict tool.
const submitToolTestName = "emit_verdict"

// submitProvider returns a submit-result provider whose tool carries
// submitToolTestName and a non-nil handler, so the executor treats it as the
// configured terminal tool.
func submitProvider() SubmitResultProvider[*testResponse] {
	return func() (claudetool.Metadata[*testResponse], error) {
		return claudetool.Metadata[*testResponse]{
			Definition: anthropic.ToolParam{Name: submitToolTestName},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[*testResponse], **testResponse) map[string]any {
				return map[string]any{}
			},
		}, nil
	}
}

// forcedToolName returns the tool name a forced tool_choice points at, or ""
// when the assembled params do not force a specific tool.
func forcedToolName(p anthropic.MessageNewParams) string {
	if p.ToolChoice.OfTool == nil {
		return ""
	}
	return p.ToolChoice.OfTool.Name
}

// TestForceSubmitToolChoiceRejectedWithThinking asserts the option is
// incompatible with extended thinking: the API forbids a forced tool_choice
// while thinking is active, so construction must fail rather than emit a
// request the API rejects at runtime.
func TestForceSubmitToolChoiceRejectedWithThinking(t *testing.T) {
	t.Parallel()

	_, err := newTestExecutorErr(t,
		WithSubmitResultProvider[*testBindable, *testResponse](submitProvider()),
		WithThinking[*testBindable, *testResponse](2048),
		WithForceSubmitToolChoice[*testBindable, *testResponse](""),
	)
	if err == nil {
		t.Fatal("New() with WithForceSubmitToolChoice and WithThinking: want error, got nil")
	}
}

// TestForceSubmitToolChoiceRequiresSubmitTool asserts the option is a no-op
// when no terminal submit tool is configured: there is no tool to force toward,
// so turn 1 stays at auto.
func TestForceSubmitToolChoiceRequiresSubmitTool(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithForceSubmitToolChoice[*testBindable, *testResponse](""),
	)
	params, _, err := exec.assembleParams("payload", "", twoTools())
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}
	if got := forcedToolName(params); got != "" {
		t.Errorf("turn-1 forced tool = %q without submit tool, want none", got)
	}
}

// TestForceSubmitToolChoiceForcesTurnOneNoDeferral asserts that with the option
// set, a submit tool configured, and no deferral gate tool registered, turn 1
// forces the submit tool directly — eliminating the reactive-redirect turn.
func TestForceSubmitToolChoiceForcesTurnOneNoDeferral(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithSubmitResultProvider[*testBindable, *testResponse](submitProvider()),
		WithForceSubmitToolChoice[*testBindable, *testResponse](""),
	)
	params, _, err := exec.assembleParams("payload", "", twoTools())
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}
	if got, want := forcedToolName(params), submitToolTestName; got != want {
		t.Errorf("turn-1 forced tool = %q, want %q", got, want)
	}
}

// TestForceSubmitToolChoiceDefersWhenGateToolRegistered asserts that when a
// deferral gate tool (e.g. fetch_deferred_evidence) is registered, turn 1 stays
// at auto so the model can fetch deferred evidence first. The force is applied
// only on a later turn, after the gate tool resolves.
func TestForceSubmitToolChoiceDefersWhenGateToolRegistered(t *testing.T) {
	t.Parallel()

	tools := twoTools()
	tools["fetch_deferred_evidence"] = claudetool.Metadata[*testResponse]{
		Definition: anthropic.ToolParam{Name: "fetch_deferred_evidence"},
	}

	exec := newTestExecutor(t,
		WithSubmitResultProvider[*testBindable, *testResponse](submitProvider()),
		WithForceSubmitToolChoice[*testBindable, *testResponse]("fetch_deferred_evidence"),
	)
	params, _, err := exec.assembleParams("payload", "", tools)
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}
	if got := forcedToolName(params); got != "" {
		t.Errorf("turn-1 forced tool = %q with gate tool registered, want none (auto)", got)
	}
}

// TestForceSubmitToolChoiceForcesTurnOneWhenGateToolAbsent asserts that naming a
// deferral gate tool that is NOT registered still forces turn 1: the deferral
// only applies when the gate tool is actually present.
func TestForceSubmitToolChoiceForcesTurnOneWhenGateToolAbsent(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithSubmitResultProvider[*testBindable, *testResponse](submitProvider()),
		WithForceSubmitToolChoice[*testBindable, *testResponse]("fetch_deferred_evidence"),
	)
	params, _, err := exec.assembleParams("payload", "", twoTools())
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}
	if got, want := forcedToolName(params), submitToolTestName; got != want {
		t.Errorf("turn-1 forced tool = %q with gate tool absent, want %q", got, want)
	}
}

// TestForceSubmitToolChoiceOffByDefault asserts the lever is off by default:
// without WithForceSubmitToolChoice, turn 1 stays at auto even with a submit
// tool configured, preserving the reactive behavior for callers who do not opt
// in.
func TestForceSubmitToolChoiceOffByDefault(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithSubmitResultProvider[*testBindable, *testResponse](submitProvider()),
	)
	params, _, err := exec.assembleParams("payload", "", twoTools())
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}
	if got := forcedToolName(params); got != "" {
		t.Errorf("turn-1 forced tool = %q with option off, want none", got)
	}
}
