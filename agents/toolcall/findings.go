/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/chainguard-dev/clog"
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
	for name, t := range findingToolDefs[Resp](cb.FindingCallbacks) {
		tools[name] = t.ClaudeMetadata()
	}
	return tools
}

func (p findingToolsProvider[Resp, T]) GoogleTools(cb FindingTools[T]) map[string]googletool.Metadata[Resp] {
	tools := p.baseProvider.GoogleTools(cb.base)
	for name, t := range findingToolDefs[Resp](cb.FindingCallbacks) {
		tools[name] = t.GoogleMetadata()
	}
	return tools
}

func findingToolDefs[Resp any](cb callbacks.FindingCallbacks) map[string]Tool[Resp] {
	defs := make(map[string]Tool[Resp])

	if cb.HasGetDetails() {
		defs["get_finding_details"] = getFindingDetailsTool[Resp](cb.GetDetails)
	}
	if cb.HasGetLogs() {
		defs["get_finding_logs"] = getFindingLogsTool[Resp](cb.GetLogs)
	}

	return defs
}

func getFindingDetailsTool[Resp any](getDetails func(context.Context, callbacks.FindingKind, string) (string, error)) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "get_finding_details",
			Description: "Get detailed information about a finding (CI failure, etc.) to understand what went wrong.",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why you need details about this finding.", Required: true},
				{Name: "kind", Type: "string", Description: "The kind of finding (from the request's findings list)", Required: true},
				{Name: "identifier", Type: "string", Description: "The identifier of the finding (from the request's findings list)", Required: true},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			kind, errResp := Param[string](call, trace, "kind")
			if errResp != nil {
				return errResp
			}

			identifier, errResp := Param[string](call, trace, "identifier")
			if errResp != nil {
				return errResp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier})

			details, err := getDetails(ctx, callbacks.FindingKind(kind), identifier)
			if err != nil {
				log.With("kind", kind).With("identifier", identifier).With("error", err).Error("Failed to get finding details")
				result := params.ErrorWithContext(err, map[string]any{"kind": kind, "identifier": identifier})
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
		},
	}
}

func getFindingLogsTool[Resp any](getLogs func(context.Context, callbacks.FindingKind, string) (string, error)) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "get_finding_logs",
			Description: "Fetch the full logs for a finding (e.g., GitHub Actions job logs). Use this to see the complete output of a failed CI check.",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why you need the logs for this finding.", Required: true},
				{Name: "kind", Type: "string", Description: "The kind of finding (from the request's findings list)", Required: true},
				{Name: "identifier", Type: "string", Description: "The identifier of the finding (from the request's findings list)", Required: true},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			kind, errResp := Param[string](call, trace, "kind")
			if errResp != nil {
				return errResp
			}

			identifier, errResp := Param[string](call, trace, "identifier")
			if errResp != nil {
				return errResp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier})

			logs, err := getLogs(ctx, callbacks.FindingKind(kind), identifier)
			if err != nil {
				log.With("kind", kind).With("identifier", identifier).With("error", err).Error("Failed to get finding logs")
				result := params.ErrorWithContext(err, map[string]any{"kind": kind, "identifier": identifier})
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
		},
	}
}
