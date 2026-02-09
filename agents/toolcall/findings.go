/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"errors"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"google.golang.org/genai"
)

// FindingTools wraps a base tools type and adds finding callbacks.
type FindingTools[T any] struct {
	base T
	callbacks.FindingCallbacks
}

// NewFindingTools creates a FindingTools wrapping the given base tools.
func NewFindingTools[T any](base T, cb callbacks.FindingCallbacks) FindingTools[T] {
	return FindingTools[T]{base: base, FindingCallbacks: cb}
}

// findingToolsProvider wraps a base ToolProvider and adds finding tools.
type findingToolsProvider[Resp, T any] struct {
	baseProvider ToolProvider[Resp, T]
}

var _ ToolProvider[any, FindingTools[any]] = (*findingToolsProvider[any, any])(nil)

// NewFindingToolsProvider creates a provider that adds finding tools
// (get_finding_details, get_finding_logs) on top of the base provider's tools.
// The finding tools are only added if the corresponding callbacks are available.
func NewFindingToolsProvider[Resp, T any](base ToolProvider[Resp, T]) ToolProvider[Resp, FindingTools[T]] {
	return findingToolsProvider[Resp, T]{baseProvider: base}
}

func (p findingToolsProvider[Resp, T]) ClaudeTools(cb FindingTools[T]) map[string]claudetool.Metadata[Resp] {
	tools := p.baseProvider.ClaudeTools(cb.base)

	if cb.HasGetDetails() {
		tools["get_finding_details"] = claudetool.Metadata[Resp]{
			Definition: anthropic.ToolParam{
				Name:        "get_finding_details",
				Description: anthropic.String("Get detailed information about a finding (CI failure, etc.) to understand what went wrong."),
				InputSchema: anthropic.ToolInputSchemaParam{
					Type: "object",
					Properties: map[string]any{
						"reasoning": map[string]any{
							"type":        "string",
							"description": "Explain why you need details about this finding.",
						},
						"kind": map[string]any{
							"type":        "string",
							"description": "The kind of finding (from the request's findings list)",
						},
						"identifier": map[string]any{
							"type":        "string",
							"description": "The identifier of the finding (from the request's findings list)",
						},
					},
					Required: []string{"reasoning", "kind", "identifier"},
				},
			},
			Handler: claudeGetFindingDetailsHandler[Resp](cb.GetDetails),
		}
	}

	if cb.HasGetLogs() {
		tools["get_finding_logs"] = claudetool.Metadata[Resp]{
			Definition: anthropic.ToolParam{
				Name:        "get_finding_logs",
				Description: anthropic.String("Fetch the full logs for a finding (e.g., GitHub Actions job logs). Use this to see the complete output of a failed CI check."),
				InputSchema: anthropic.ToolInputSchemaParam{
					Type: "object",
					Properties: map[string]any{
						"reasoning": map[string]any{
							"type":        "string",
							"description": "Explain why you need the logs for this finding.",
						},
						"kind": map[string]any{
							"type":        "string",
							"description": "The kind of finding (from the request's findings list)",
						},
						"identifier": map[string]any{
							"type":        "string",
							"description": "The identifier of the finding (from the request's findings list)",
						},
					},
					Required: []string{"reasoning", "kind", "identifier"},
				},
			},
			Handler: claudeGetFindingLogsHandler[Resp](cb.GetLogs),
		}
	}

	return tools
}

func (p findingToolsProvider[Resp, T]) GoogleTools(cb FindingTools[T]) map[string]googletool.Metadata[Resp] {
	tools := p.baseProvider.GoogleTools(cb.base)

	if cb.HasGetDetails() {
		tools["get_finding_details"] = googletool.Metadata[Resp]{
			Definition: &genai.FunctionDeclaration{
				Name:        "get_finding_details",
				Description: "Get detailed information about a finding (CI failure, etc.) to understand what went wrong.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"reasoning": {
							Type:        "string",
							Description: "Explain why you need details about this finding.",
						},
						"kind": {
							Type:        "string",
							Description: "The kind of finding (from the request's findings list)",
						},
						"identifier": {
							Type:        "string",
							Description: "The identifier of the finding (from the request's findings list)",
						},
					},
					Required: []string{"reasoning", "kind", "identifier"},
				},
			},
			Handler: googleGetFindingDetailsHandler[Resp](cb.GetDetails),
		}
	}

	if cb.HasGetLogs() {
		tools["get_finding_logs"] = googletool.Metadata[Resp]{
			Definition: &genai.FunctionDeclaration{
				Name:        "get_finding_logs",
				Description: "Fetch the full logs for a finding (e.g., GitHub Actions job logs). Use this to see the complete output of a failed CI check.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"reasoning": {
							Type:        "string",
							Description: "Explain why you need the logs for this finding.",
						},
						"kind": {
							Type:        "string",
							Description: "The kind of finding (from the request's findings list)",
						},
						"identifier": {
							Type:        "string",
							Description: "The identifier of the finding (from the request's findings list)",
						},
					},
					Required: []string{"reasoning", "kind", "identifier"},
				},
			},
			Handler: googleGetFindingLogsHandler[Resp](cb.GetLogs),
		}
	}

	return tools
}

// Claude handlers for finding tools

func claudeGetFindingDetailsHandler[Resp any](getDetails func(context.Context, callbacks.FindingKind, string) (string, error)) func(context.Context, anthropic.ToolUseBlock, *evals.Trace[Resp], *Resp) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *evals.Trace[Resp], _ *Resp) map[string]any {
		log := clog.FromContext(ctx)

		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("failed to parse params"))
			return errResp
		}

		reasoning, errResp := claudetool.Param[string](params, "reasoning")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		kind, errResp := claudetool.Param[string](params, "kind")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing kind parameter"))
			return errResp
		}

		identifier, errResp := claudetool.Param[string](params, "identifier")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing identifier parameter"))
			return errResp
		}

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, map[string]any{"kind": kind, "identifier": identifier})

		details, err := getDetails(ctx, callbacks.FindingKind(kind), identifier)
		if err != nil {
			log.With("kind", kind).With("identifier", identifier).With("error", err).Error("Failed to get finding details")
			result := claudetool.ErrorWithContext(err, map[string]any{"kind": kind, "identifier": identifier})
			tc.Complete(result, err)
			return result
		}

		result := map[string]any{
			"kind":       kind,
			"identifier": identifier,
			"details":    details,
		}
		tc.Complete(result, nil)
		return result
	}
}

func claudeGetFindingLogsHandler[Resp any](getLogs func(context.Context, callbacks.FindingKind, string) (string, error)) func(context.Context, anthropic.ToolUseBlock, *evals.Trace[Resp], *Resp) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *evals.Trace[Resp], _ *Resp) map[string]any {
		log := clog.FromContext(ctx)

		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("failed to parse params"))
			return errResp
		}

		reasoning, errResp := claudetool.Param[string](params, "reasoning")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		kind, errResp := claudetool.Param[string](params, "kind")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing kind parameter"))
			return errResp
		}

		identifier, errResp := claudetool.Param[string](params, "identifier")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing identifier parameter"))
			return errResp
		}

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, map[string]any{"kind": kind, "identifier": identifier})

		logs, err := getLogs(ctx, callbacks.FindingKind(kind), identifier)
		if err != nil {
			log.With("kind", kind).With("identifier", identifier).With("error", err).Error("Failed to get finding logs")
			result := claudetool.ErrorWithContext(err, map[string]any{"kind": kind, "identifier": identifier})
			tc.Complete(result, err)
			return result
		}

		result := map[string]any{
			"kind":       kind,
			"identifier": identifier,
			"logs":       logs,
			"size":       len(logs),
		}
		tc.Complete(result, nil)
		return result
	}
}

// Google handlers for finding tools

func googleGetFindingDetailsHandler[Resp any](getDetails func(context.Context, callbacks.FindingKind, string) (string, error)) func(context.Context, *genai.FunctionCall, *evals.Trace[Resp], *Resp) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *evals.Trace[Resp], _ *Resp) *genai.FunctionResponse {
		log := clog.FromContext(ctx)

		reasoning, errResp := googletool.Param[string](call, "reasoning")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		kind, errResp := googletool.Param[string](call, "kind")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing kind parameter"))
			return errResp
		}

		identifier, errResp := googletool.Param[string](call, "identifier")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing identifier parameter"))
			return errResp
		}

		tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier})

		details, err := getDetails(ctx, callbacks.FindingKind(kind), identifier)
		if err != nil {
			log.With("kind", kind).With("identifier", identifier).With("error", err).Error("Failed to get finding details")
			result := googletool.ErrorWithContext(call, err, map[string]any{"kind": kind, "identifier": identifier})
			tc.Complete(result.Response, err)
			return result
		}

		result := map[string]any{
			"kind":       kind,
			"identifier": identifier,
			"details":    details,
		}
		tc.Complete(result, nil)

		return &genai.FunctionResponse{
			ID:       call.ID,
			Name:     call.Name,
			Response: result,
		}
	}
}

func googleGetFindingLogsHandler[Resp any](getLogs func(context.Context, callbacks.FindingKind, string) (string, error)) func(context.Context, *genai.FunctionCall, *evals.Trace[Resp], *Resp) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *evals.Trace[Resp], _ *Resp) *genai.FunctionResponse {
		log := clog.FromContext(ctx)

		reasoning, errResp := googletool.Param[string](call, "reasoning")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		kind, errResp := googletool.Param[string](call, "kind")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing kind parameter"))
			return errResp
		}

		identifier, errResp := googletool.Param[string](call, "identifier")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing identifier parameter"))
			return errResp
		}

		tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier})

		logs, err := getLogs(ctx, callbacks.FindingKind(kind), identifier)
		if err != nil {
			log.With("kind", kind).With("identifier", identifier).With("error", err).Error("Failed to get finding logs")
			result := googletool.ErrorWithContext(call, err, map[string]any{"kind": kind, "identifier": identifier})
			tc.Complete(result.Response, err)
			return result
		}

		result := map[string]any{
			"kind":       kind,
			"identifier": identifier,
			"logs":       logs,
			"size":       len(logs),
		}
		tc.Complete(result, nil)

		return &genai.FunctionResponse{
			ID:       call.ID,
			Name:     call.Name,
			Response: result,
		}
	}
}
