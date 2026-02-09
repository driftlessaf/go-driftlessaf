/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"
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

	tools["random_number"] = claudetool.Metadata[Resp]{
		Definition: anthropic.ToolParam{
			Name:        "random_number",
			Description: anthropic.String("Generate a random number between min and max (inclusive)."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Type: "object",
				Properties: map[string]any{
					"min": map[string]any{
						"type":        "integer",
						"description": "The minimum value (inclusive)",
					},
					"max": map[string]any{
						"type":        "integer",
						"description": "The maximum value (inclusive)",
					},
				},
				Required: []string{"min", "max"},
			},
		},
		Handler: claudeRandomNumberHandler[Resp](cb.RandomNumber),
	}
	return tools
}

func (p exampleToolsProvider[Resp, T]) GoogleTools(cb ExampleTools[T]) map[string]googletool.Metadata[Resp] {
	tools := p.base.GoogleTools(cb.base)

	tools["random_number"] = googletool.Metadata[Resp]{
		Definition: &genai.FunctionDeclaration{
			Name:        "random_number",
			Description: "Generate a random number between min and max (inclusive).",
			Parameters: &genai.Schema{
				Type: "object",
				Properties: map[string]*genai.Schema{
					"min": {
						Type:        "integer",
						Description: "The minimum value (inclusive)",
					},
					"max": {
						Type:        "integer",
						Description: "The maximum value (inclusive)",
					},
				},
				Required: []string{"min", "max"},
			},
		},
		Handler: googleRandomNumberHandler[Resp](cb.RandomNumber),
	}
	return tools
}

// claudeRandomNumberHandler creates a Claude handler for the random_number tool.
func claudeRandomNumberHandler[Resp any](randomFn func(context.Context, int, int) (int, error)) func(context.Context, anthropic.ToolUseBlock, *evals.Trace[Resp], *Resp) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *evals.Trace[Resp], _ *Resp) map[string]any {
		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("failed to parse params"))
			return errResp
		}

		minVal, errResp := claudetool.Param[float64](params, "min")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing min parameter"))
			return errResp
		}

		maxVal, errResp := claudetool.Param[float64](params, "max")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing max parameter"))
			return errResp
		}

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, map[string]any{"min": minVal, "max": maxVal})

		result, err := randomFn(ctx, int(minVal), int(maxVal))
		if err != nil {
			output := claudetool.ErrorWithContext(err, map[string]any{"min": minVal, "max": maxVal})
			tc.Complete(output, err)
			return output
		}

		output := map[string]any{"result": result}
		tc.Complete(output, nil)
		return output
	}
}

// googleRandomNumberHandler creates a Google handler for the random_number tool.
func googleRandomNumberHandler[Resp any](randomFn func(context.Context, int, int) (int, error)) func(context.Context, *genai.FunctionCall, *evals.Trace[Resp], *Resp) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *evals.Trace[Resp], _ *Resp) *genai.FunctionResponse {
		minVal, errResp := googletool.Param[float64](call, "min")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing min parameter"))
			return errResp
		}

		maxVal, errResp := googletool.Param[float64](call, "max")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing max parameter"))
			return errResp
		}

		tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"min": minVal, "max": maxVal})

		result, err := randomFn(ctx, int(minVal), int(maxVal))
		if err != nil {
			resp := googletool.ErrorWithContext(call, err, map[string]any{"min": minVal, "max": maxVal})
			tc.Complete(resp.Response, err)
			return resp
		}

		output := map[string]any{"result": result}
		tc.Complete(output, nil)
		return &genai.FunctionResponse{
			ID:       call.ID,
			Name:     call.Name,
			Response: output,
		}
	}
}
