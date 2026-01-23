/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/metrics"
)

// NewVertex creates a new Interface instance by delegating to the appropriate
// implementation based on the model name. Claude models use Anthropic SDK,
// Gemini models use Google's Generative AI SDK.
// Accepts optional executor options that will be passed through to the underlying executor.
func NewVertex(ctx context.Context, projectID, region, modelName string, opts ...any) (Interface, error) {
	modelLower := strings.ToLower(modelName)

	// Delegate to Claude implementation for Claude models
	if strings.HasPrefix(modelLower, "claude-") {
		// Extract Claude options
		claudeOpts := make([]claudeexecutor.Option[*Request, *Judgement], 0, len(opts))
		for _, opt := range opts {
			if claudeOpt, ok := opt.(claudeexecutor.Option[*Request, *Judgement]); ok {
				claudeOpts = append(claudeOpts, claudeOpt)
			}
		}
		return newClaude(ctx, projectID, region, modelName, claudeOpts...)
	}

	// Delegate to Google implementation for Gemini models
	if strings.HasPrefix(modelLower, "gemini-") {
		// Extract Google options
		googleOpts := make([]googleexecutor.Option[*Request, *Judgement], 0, len(opts))
		for _, opt := range opts {
			if googleOpt, ok := opt.(googleexecutor.Option[*Request, *Judgement]); ok {
				googleOpts = append(googleOpts, googleOpt)
			}
		}
		return newGoogle(ctx, projectID, region, modelName, googleOpts...)
	}

	return nil, fmt.Errorf("unsupported model: %s (expected claude-* or gemini-*)", modelName)
}

// NewVertexWithEnricher creates a new Interface instance with an attribute enricher for metrics.
// This is a convenience function for the common case of passing an enricher.
func NewVertexWithEnricher(ctx context.Context, projectID, region, modelName string, enricher metrics.AttributeEnricher) (Interface, error) {
	modelLower := strings.ToLower(modelName)

	// Delegate to Claude implementation for Claude models
	if strings.HasPrefix(modelLower, "claude-") {
		return newClaude(ctx, projectID, region, modelName,
			claudeexecutor.WithAttributeEnricher[*Request, *Judgement](enricher))
	}

	// Delegate to Google implementation for Gemini models
	if strings.HasPrefix(modelLower, "gemini-") {
		return newGoogle(ctx, projectID, region, modelName,
			googleexecutor.WithAttributeEnricher[*Request, *Judgement](enricher))
	}

	return nil, fmt.Errorf("unsupported model: %s (expected claude-* or gemini-*)", modelName)
}
