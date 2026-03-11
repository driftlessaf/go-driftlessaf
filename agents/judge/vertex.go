/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
)

// NewVertex creates a new Interface instance by delegating to the appropriate
// implementation based on the model name. Claude models use Anthropic SDK,
// Gemini models use Google's Generative AI SDK.
// Accepts optional executor options that will be passed through to the underlying executor.
func NewVertex(ctx context.Context, projectID, region, modelName string, opts ...any) (Interface, error) {
	return NewVertexWithLabels(ctx, projectID, region, modelName, nil, opts...)
}

// NewVertexWithLabels creates a new Interface with GCP resource labels for billing attribution.
// model_name is always added to the labels automatically. The labels map is copied, not mutated.
func NewVertexWithLabels(ctx context.Context, projectID, region, modelName string, labels map[string]string, opts ...any) (Interface, error) {
	modelLower := strings.ToLower(modelName)

	// Copy to avoid mutating the caller's map, and inject model_name.
	merged := make(map[string]string, len(labels)+1)
	maps.Copy(merged, labels)
	merged["model_name"] = modelLower

	if strings.HasPrefix(modelLower, "claude-") {
		claudeOpts := []claudeexecutor.Option[*Request, *Judgement]{
			claudeexecutor.WithResourceLabels[*Request, *Judgement](merged),
		}
		for _, opt := range opts {
			if claudeOpt, ok := opt.(claudeexecutor.Option[*Request, *Judgement]); ok {
				claudeOpts = append(claudeOpts, claudeOpt)
			}
		}
		return newClaude(ctx, projectID, region, modelName, claudeOpts...)
	}

	if strings.HasPrefix(modelLower, "gemini-") {
		googleOpts := []googleexecutor.Option[*Request, *Judgement]{
			googleexecutor.WithResourceLabels[*Request, *Judgement](merged),
		}
		for _, opt := range opts {
			if googleOpt, ok := opt.(googleexecutor.Option[*Request, *Judgement]); ok {
				googleOpts = append(googleOpts, googleOpt)
			}
		}
		return newGoogle(ctx, projectID, region, modelName, googleOpts...)
	}

	return nil, fmt.Errorf("unsupported model: %s (expected claude-* or gemini-*)", modelName)
}
