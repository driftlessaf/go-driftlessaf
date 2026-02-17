/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"os"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
)

// ExampleTools wraps a base tools type and adds example tool functionality.
// This demonstrates how to extend the tool composition pattern with custom methods.
type ExampleTools[T any] struct {
	base T
}

// NewExampleTools creates an ExampleTools wrapping the given base tools.
func NewExampleTools[T any](base T) ExampleTools[T] {
	return ExampleTools[T]{base: base}
}

// RandomNumber generates a cryptographically secure random number between min and max (inclusive).
func (ExampleTools[T]) RandomNumber(_ context.Context, minVal, maxVal int) (int, error) {
	if minVal >= maxVal {
		return 0, fmt.Errorf("min (%d) must be less than max (%d)", minVal, maxVal)
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxVal-minVal)+1))
	if err != nil {
		return 0, err
	}
	return minVal + int(n.Int64()), nil
}

// exampleToolsProvider wraps a base provider and adds the random_number tool.
type exampleToolsProvider[Resp, T any] struct {
	base ToolProvider[Resp, T]
}

// NewExampleToolsProvider creates a provider that adds example tools to a base provider.
func NewExampleToolsProvider[Resp, T any](base ToolProvider[Resp, T]) ToolProvider[Resp, ExampleTools[T]] {
	return exampleToolsProvider[Resp, T]{base: base}
}

func (p exampleToolsProvider[Resp, T]) ClaudeTools(cb ExampleTools[T]) map[string]claudetool.Metadata[Resp] {
	tools := p.base.ClaudeTools(cb.base)
	t := randomNumberTool[Resp](cb.RandomNumber)
	tools["random_number"] = t.ClaudeMetadata()
	return tools
}

func (p exampleToolsProvider[Resp, T]) GoogleTools(cb ExampleTools[T]) map[string]googletool.Metadata[Resp] {
	tools := p.base.GoogleTools(cb.base)
	t := randomNumberTool[Resp](cb.RandomNumber)
	tools["random_number"] = t.GoogleMetadata()
	return tools
}

func randomNumberTool[Resp any](randomFn func(context.Context, int, int) (int, error)) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "random_number",
			Description: "Generate a random number between min and max (inclusive).",
			Parameters: []Parameter{
				{Name: "min", Type: "integer", Description: "The minimum value (inclusive)", Required: true},
				{Name: "max", Type: "integer", Description: "The maximum value (inclusive)", Required: true},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			minVal, errResp := Param[float64](call, trace, "min")
			if errResp != nil {
				return errResp
			}

			maxVal, errResp := Param[float64](call, trace, "max")
			if errResp != nil {
				return errResp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"min": minVal, "max": maxVal})

			result, err := randomFn(ctx, int(minVal), int(maxVal))
			if err != nil {
				output := params.ErrorWithContext(err, map[string]any{"min": minVal, "max": maxVal})
				tc.Complete(output, err)
				return output
			}

			output := map[string]any{"result": result}
			tc.Complete(output, nil)
			return output
		},
	}
}

// Ensure the example tools provider stack compiles and works for both providers.
var _ = func() {
	type Response struct{}

	base := NewEmptyToolsProvider[*Response]()
	wtProvider := NewWorktreeToolsProvider[*Response, EmptyTools](base)
	findingProvider := NewFindingToolsProvider[*Response, WorktreeTools[EmptyTools]](wtProvider)
	exampleProvider := NewExampleToolsProvider[*Response, FindingTools[WorktreeTools[EmptyTools]]](findingProvider)

	wt := callbacks.WorktreeCallbacks{
		ReadFile: func(_ context.Context, _ string, _ int64, _ int) (callbacks.ReadResult, error) {
			return callbacks.ReadResult{}, nil
		},
		WriteFile:     func(_ context.Context, _, _ string, _ os.FileMode) error { return nil },
		DeleteFile:    func(_ context.Context, _ string) error { return nil },
		MoveFile:      func(_ context.Context, _, _ string) error { return nil },
		CopyFile:      func(_ context.Context, _, _ string) error { return nil },
		CreateSymlink: func(_ context.Context, _, _ string) error { return nil },
		Chmod:         func(_ context.Context, _ string, _ os.FileMode) error { return nil },
		EditFile: func(_ context.Context, _, _, _ string, _ bool) (callbacks.EditResult, error) {
			return callbacks.EditResult{}, nil
		},
		ListDirectory: func(_ context.Context, _, _ string, _, _ int) (callbacks.ListResult, error) {
			return callbacks.ListResult{}, nil
		},
		SearchCodebase: func(_ context.Context, _, _, _ string, _, _ int) (callbacks.SearchResult, error) {
			return callbacks.SearchResult{}, nil
		},
	}
	fc := callbacks.FindingCallbacks{
		GetDetails: func(_ context.Context, _ callbacks.FindingKind, _ string) (string, error) { return "", nil },
		GetLogs:    func(_ context.Context, _ callbacks.FindingKind, _ string) (string, error) { return "", nil },
	}
	tools := NewExampleTools(NewFindingTools(NewWorktreeTools(EmptyTools{}, wt), fc))

	// Both methods should compile.
	_ = exampleProvider.ClaudeTools(tools)
	_ = exampleProvider.GoogleTools(tools)
}
