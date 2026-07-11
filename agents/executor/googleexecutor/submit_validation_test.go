/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// newSubmitTestExecutor builds an executor whose submit tool parses
// {reasoning, result:{result}} calls into testResponse, with the given
// validators. The client is never used because evaluateSubmission makes no
// network calls.
func newSubmitTestExecutor(t *testing.T, validators ...callbacks.ResultValidator[testResponse]) *executor[*testBindable, testResponse] {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	opts := []Option[*testBindable, testResponse]{
		WithSubmitResultProvider[*testBindable, testResponse](func() (googletool.SubmitMetadata[testResponse], error) {
			return googletool.SubmitMetadata[testResponse]{
				Definition: &genai.FunctionDeclaration{Name: "submit_result"},
				Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[testResponse]) toolcall.SubmitOutcome[testResponse] {
					answer, _ := call.Args["answer"].(string)
					return toolcall.SubmitOutcome[testResponse]{
						Accepted:   true,
						Response:   testResponse{Result: answer},
						Reasoning:  "because",
						ToolResult: map[string]any{"success": true},
					}
				},
			}, nil
		}),
	}
	for _, v := range validators {
		opts = append(opts, WithResultValidator[*testBindable, testResponse](v))
	}

	exec, err := New[*testBindable, testResponse](nil, prompt, opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return exec.(*executor[*testBindable, testResponse])
}

func TestEvaluateSubmissionCommitsWhenValidatorsPass(t *testing.T) {
	t.Parallel()

	exec := newSubmitTestExecutor(t, func(context.Context, testResponse, string) ([]callbacks.Finding, error) {
		return nil, nil
	})

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[testResponse](ctx, "prompt")

	var result testResponse
	toolResult, committed, err := exec.evaluateSubmission(ctx, &genai.FunctionCall{
		ID: "c1", Name: "submit_result", Args: map[string]any{"answer": "done"},
	}, trace, &result)
	if err != nil {
		t.Fatalf("evaluateSubmission: got = %v, want = nil", err)
	}
	if !committed {
		t.Error("committed: got = false, want = true")
	}
	if got, want := result.Result, "done"; got != want {
		t.Errorf("result: got = %q, want = %q", got, want)
	}
	if success, _ := toolResult["success"].(bool); !success {
		t.Errorf("tool result: got = %#v, want = success:true", toolResult)
	}
}

func TestEvaluateSubmissionZeroValueResponseCommits(t *testing.T) {
	t.Parallel()

	exec := newSubmitTestExecutor(t)

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[testResponse](ctx, "prompt")

	var result testResponse
	_, committed, err := exec.evaluateSubmission(ctx, &genai.FunctionCall{
		ID: "c1", Name: "submit_result", Args: map[string]any{"answer": ""},
	}, trace, &result)
	if err != nil {
		t.Fatalf("evaluateSubmission: got = %v, want = nil", err)
	}
	if !committed {
		t.Error("committed: got = false, want = true (zero-value submission must still commit)")
	}
}

func TestEvaluateSubmissionRejectionIsNotCommitted(t *testing.T) {
	t.Parallel()

	exec := newSubmitTestExecutor(t, func(context.Context, testResponse, string) ([]callbacks.Finding, error) {
		return []callbacks.Finding{{
			Kind:       callbacks.FindingKindReview,
			Identifier: "bad-result",
			Details:    "the result is unacceptable",
		}}, nil
	})

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[testResponse](ctx, "prompt")

	var result testResponse
	toolResult, committed, err := exec.evaluateSubmission(ctx, &genai.FunctionCall{
		ID: "c1", Name: "submit_result", Args: map[string]any{"answer": "done"},
	}, trace, &result)
	if err != nil {
		t.Fatalf("evaluateSubmission: got = %v, want = nil", err)
	}
	if committed {
		t.Error("committed: got = true, want = false")
	}
	if got, want := result.Result, ""; got != want {
		t.Errorf("result must stay unset on rejection: got = %q, want = %q", got, want)
	}
	errText, _ := toolResult["error"].(string)
	if !strings.Contains(errText, "1 finding(s)") {
		t.Errorf("rejection tool result: got = %#v, want error mentioning findings", toolResult)
	}
}

func TestEvaluateSubmissionValidatorErrorFailsLoud(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("judge unavailable")
	exec := newSubmitTestExecutor(t, func(context.Context, testResponse, string) ([]callbacks.Finding, error) {
		return nil, wantErr
	})

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[testResponse](ctx, "prompt")

	var result testResponse
	_, committed, err := exec.evaluateSubmission(ctx, &genai.FunctionCall{
		ID: "c1", Name: "submit_result", Args: map[string]any{"answer": "done"},
	}, trace, &result)
	if !errors.Is(err, wantErr) {
		t.Fatalf("evaluateSubmission error: got = %v, want = %v", err, wantErr)
	}
	if committed {
		t.Error("committed: got = true, want = false")
	}
}

func TestEvaluateSubmissionUnacceptedOutcomePassesThrough(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	exec, err := New[*testBindable, testResponse](nil, prompt,
		WithSubmitResultProvider[*testBindable, testResponse](func() (googletool.SubmitMetadata[testResponse], error) {
			return googletool.SubmitMetadata[testResponse]{
				Definition: &genai.FunctionDeclaration{Name: "submit_result"},
				Handler: func(context.Context, *genai.FunctionCall, *agenttrace.Trace[testResponse]) toolcall.SubmitOutcome[testResponse] {
					return toolcall.SubmitOutcome[testResponse]{ToolResult: map[string]any{"error": "bad payload"}}
				},
			}, nil
		}),
		// A validator that would reject everything — it must not run for a
		// submission that never parsed.
		WithResultValidator[*testBindable, testResponse](func(context.Context, testResponse, string) ([]callbacks.Finding, error) {
			return nil, errors.New("validator must not run for unaccepted outcomes")
		}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	e := exec.(*executor[*testBindable, testResponse])

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[testResponse](ctx, "prompt")

	var result testResponse
	toolResult, committed, err := e.evaluateSubmission(ctx, &genai.FunctionCall{
		ID: "c1", Name: "submit_result", Args: map[string]any{},
	}, trace, &result)
	if err != nil {
		t.Fatalf("evaluateSubmission: got = %v, want = nil", err)
	}
	if committed {
		t.Error("committed: got = true, want = false")
	}
	if got, want := toolResult["error"], "bad payload"; got != want {
		t.Errorf("tool result error: got = %v, want = %v", got, want)
	}
}

func TestSubmitToolNameDefaults(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	// No submit tool registered: empty name.
	bare, err := New[*testBindable, testResponse](nil, prompt)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := bare.(*executor[*testBindable, testResponse]).submitToolName(); got != "" {
		t.Errorf("submitToolName without submit tool: got = %q, want = %q", got, "")
	}

	// Registered without an explicit name: default.
	unnamed, err := New[*testBindable, testResponse](nil, prompt,
		WithSubmitResultProvider[*testBindable, testResponse](func() (googletool.SubmitMetadata[testResponse], error) {
			return googletool.SubmitMetadata[testResponse]{
				Handler: func(context.Context, *genai.FunctionCall, *agenttrace.Trace[testResponse]) toolcall.SubmitOutcome[testResponse] {
					return toolcall.SubmitOutcome[testResponse]{}
				},
			}, nil
		}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got, want := unnamed.(*executor[*testBindable, testResponse]).submitToolName(), "submit_result"; got != want {
		t.Errorf("submitToolName default: got = %q, want = %q", got, want)
	}
}
