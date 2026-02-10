/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

// ExecutionContext provides reconciler-level context for agent executions.
// This context is used to enrich metrics with labels for tracking token usage
// and tool calls per reconciler (PR, path, etc.).
type ExecutionContext struct {
	ReconcilerKey  string `json:"reconciler_key,omitempty"`  // Primary identifier: "pr:chainguard-dev/enterprise-packages/41025" or "path:chainguard-dev/mono/main/images/nginx"
	ReconcilerType string `json:"reconciler_type,omitempty"` // Type of reconciler: "pr" or "path"
	CommitSHA      string `json:"commit_sha,omitempty"`      // Git commit SHA (optional, for git-based reconcilers)
	TurnNumber     int    `json:"turn_number,omitempty"`     // Turn number for multi-turn agents (optional, 1, 2, 3, ...)
}

// Repository extracts the repository from the reconciler key.
// For "pr:chainguard-dev/enterprise-packages/41025" returns "chainguard-dev/enterprise-packages"
// For "path:chainguard-dev/mono/main/images/nginx" returns "chainguard-dev/mono"
// Returns empty string if the format is invalid.
func (e ExecutionContext) Repository() string {
	if e.ReconcilerKey == "" {
		return ""
	}

	// Split at colon to get the identifier part
	_, identifier, found := strings.Cut(e.ReconcilerKey, ":")
	if !found {
		return ""
	}

	// Find the second slash to extract "owner/repo"
	firstSlash := strings.IndexByte(identifier, '/')
	if firstSlash == -1 {
		return ""
	}

	secondSlash := strings.IndexByte(identifier[firstSlash+1:], '/')
	if secondSlash == -1 {
		return ""
	}

	return identifier[:firstSlash+1+secondSlash]
}

// EnrichAttributes adds execution context attributes to the provided base attributes.
// This is used to enrich metrics with reconciler context using only BOUNDED labels.
//
// Note: reconciler_key and commit_sha are NOT included in metrics to prevent unbounded
// cardinality (every PR and commit creates a new time series). These fields remain in
// the ExecutionContext for traces where cardinality is not a concern. Use trace exemplars
// to link from aggregated metrics to detailed per-PR traces.
func (e ExecutionContext) EnrichAttributes(baseAttrs []attribute.KeyValue) []attribute.KeyValue {
	// Pre-allocate for base + up to 3 additional attributes
	attrs := make([]attribute.KeyValue, len(baseAttrs), len(baseAttrs)+3)
	copy(attrs, baseAttrs)

	// Add reconciler type (bounded: "pr" or "path")
	if e.ReconcilerType != "" {
		attrs = append(attrs, attribute.String("reconciler_type", e.ReconcilerType))
	}

	// Extract and add repository from reconciler_key for aggregation
	// This is bounded: ~100-500 repositories vs unlimited PRs
	if repo := e.Repository(); repo != "" {
		attrs = append(attrs, attribute.String("repository", repo))
	}

	// Add turn number (bounded: typically 0-10 for multi-turn agents)
	attrs = append(attrs, attribute.Int("turn", e.TurnNumber))

	return attrs
}

// contextKey is used for storing execution context in context.Context
type contextKey string

const executionContextKey contextKey = "execution_context"

// WithExecutionContext adds execution context to the Go context
func WithExecutionContext(ctx context.Context, execCtx ExecutionContext) context.Context {
	return context.WithValue(ctx, executionContextKey, execCtx)
}

// GetExecutionContext retrieves execution context from the Go context
func GetExecutionContext(ctx context.Context) ExecutionContext {
	if val := ctx.Value(executionContextKey); val != nil {
		if execCtx, ok := val.(ExecutionContext); ok {
			return execCtx
		}
	}
	return ExecutionContext{}
}
