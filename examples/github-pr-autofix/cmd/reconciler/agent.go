/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/examples/prvalidation"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v75/github"
)

// newPRFixerAgent creates a new Claude executor for PR fixing
func newPRFixerAgent(ctx context.Context, cfg *config) (claudeexecutor.Interface[*PRContext, *PRFixResult], error) {
	// Create client with Vertex AI authentication
	client := anthropic.NewClient(
		vertex.WithGoogleAuth(ctx, cfg.GCPRegion, cfg.GCPProjectID),
	)

	// Create executor with options
	return claudeexecutor.New[*PRContext, *PRFixResult](
		client,
		userPrompt,
		claudeexecutor.WithModel[*PRContext, *PRFixResult](cfg.ClaudeModel),
		claudeexecutor.WithMaxTokens[*PRContext, *PRFixResult](8192),
		claudeexecutor.WithTemperature[*PRContext, *PRFixResult](0.1),
		claudeexecutor.WithSystemInstructions[*PRContext, *PRFixResult](systemInstructions),
		claudeexecutor.WithSubmitResultProvider[*PRContext, *PRFixResult](submitresult.ClaudeToolForResponse[*PRFixResult]),
	)
}

// createTools creates the tool definitions for the agent
func createTools(_ context.Context, gh *github.Client, owner, repo string, prNumber int) map[string]claudetool.Metadata[*PRFixResult] {
	return map[string]claudetool.Metadata[*PRFixResult]{
		"update_pr_title": {
			Definition: anthropic.ToolParam{
				Name: "update_pr_title",
				Description: anthropic.String(`Update the PR title to fix validation issues.

Format: <type>: <description> or <type>(<scope>): <description>
CRITICAL: There MUST be a space after the colon!

Examples of VALID titles:
- "docs: update README with setup instructions"
- "feat(api): add user authentication endpoint"
- "fix: resolve memory leak in cache module"

Examples of INVALID titles:
- "docs:update" (no space after colon)
- "update readme" (missing type)

Valid types: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert
Max length: 72 characters`),
				InputSchema: anthropic.ToolInputSchemaParam{
					Type: "object",
					Properties: map[string]any{
						"reasoning": map[string]any{
							"type":        "string",
							"description": "Explain why this title change fixes the issue",
						},
						"new_title": map[string]any{
							"type":        "string",
							"description": "The new PR title in conventional commit format",
						},
					},
					Required: []string{"reasoning", "new_title"},
				},
			},
			Handler: func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *evals.Trace[*PRFixResult], result **PRFixResult) map[string]any {
				return executeUpdateTitleTool(ctx, toolUse, gh, owner, repo, prNumber, trace)
			},
		},
		"update_pr_description": {
			Definition: anthropic.ToolParam{
				Name: "update_pr_description",
				Description: anthropic.String(`Update the PR description/body to fix validation issues.
The description should be meaningful and at least 20 characters.
Preserve any existing content that is useful.`),
				InputSchema: anthropic.ToolInputSchemaParam{
					Type: "object",
					Properties: map[string]any{
						"reasoning": map[string]any{
							"type":        "string",
							"description": "Explain why this description change fixes the issue",
						},
						"new_description": map[string]any{
							"type":        "string",
							"description": "The new PR description",
						},
					},
					Required: []string{"reasoning", "new_description"},
				},
			},
			Handler: func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *evals.Trace[*PRFixResult], result **PRFixResult) map[string]any {
				return executeUpdateDescriptionTool(ctx, toolUse, gh, owner, repo, prNumber, trace)
			},
		},
	}
}

// executeUpdateTitleTool handles the update_pr_title tool call
func executeUpdateTitleTool(ctx context.Context, toolUse anthropic.ToolUseBlock, gh *github.Client, owner, repo string, prNumber int, trace *evals.Trace[*PRFixResult]) map[string]any {
	log := clog.FromContext(ctx)

	// Create parameter extractor
	params, errResp := claudetool.NewParams(toolUse)
	if errResp != nil {
		trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("parameter error"))
		return errResp
	}

	// Extract reasoning
	reasoning, errResp := claudetool.Param[string](params, "reasoning")
	if errResp != nil {
		trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("parameter error"))
		return errResp
	}
	log.With("reasoning", reasoning).Info("Tool call reasoning")

	// Extract new_title
	newTitle, errResp := claudetool.Param[string](params, "new_title")
	if errResp != nil {
		trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("parameter error"))
		return errResp
	}

	// Start tool call trace
	tc := trace.StartToolCall(toolUse.ID, toolUse.Name, params.RawInputs())

	// Validate title length
	if len(newTitle) > 72 {
		result := claudetool.Error("title must be under 72 characters, got %d", len(newTitle))
		tc.Complete(result, fmt.Errorf("title too long: %d chars", len(newTitle)))
		return result
	}

	// Validate conventional commit format
	if !prvalidation.ConventionalCommitRegex.MatchString(newTitle) {
		result := claudetool.Error("title does not match conventional commit format: %s", newTitle)
		tc.Complete(result, fmt.Errorf("invalid format"))
		return result
	}

	// Call GitHub API to update
	_, _, err := gh.PullRequests.Edit(ctx, owner, repo, prNumber, &github.PullRequest{
		Title: github.Ptr(newTitle),
	})
	if err != nil {
		log.With("error", err).Error("Failed to update PR title")
		result := claudetool.ErrorWithContext(err, map[string]any{"new_title": newTitle})
		tc.Complete(result, err)
		return result
	}

	result := map[string]any{
		"success":   true,
		"message":   "PR title updated successfully",
		"new_title": newTitle,
	}
	tc.Complete(result, nil)
	return result
}

// executeUpdateDescriptionTool handles the update_pr_description tool call
func executeUpdateDescriptionTool(ctx context.Context, toolUse anthropic.ToolUseBlock, gh *github.Client, owner, repo string, prNumber int, trace *evals.Trace[*PRFixResult]) map[string]any {
	log := clog.FromContext(ctx)

	// Create parameter extractor
	params, errResp := claudetool.NewParams(toolUse)
	if errResp != nil {
		trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("parameter error"))
		return errResp
	}

	// Extract reasoning
	reasoning, errResp := claudetool.Param[string](params, "reasoning")
	if errResp != nil {
		trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("parameter error"))
		return errResp
	}
	log.With("reasoning", reasoning).Info("Tool call reasoning")

	// Extract new_description
	newDescription, errResp := claudetool.Param[string](params, "new_description")
	if errResp != nil {
		trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("parameter error"))
		return errResp
	}

	// Start tool call trace
	tc := trace.StartToolCall(toolUse.ID, toolUse.Name, params.RawInputs())

	// Validate description length
	trimmed := strings.TrimSpace(newDescription)
	if len(trimmed) < 20 {
		result := claudetool.Error("description must be at least 20 characters, got %d", len(trimmed))
		tc.Complete(result, fmt.Errorf("description too short: %d chars", len(trimmed)))
		return result
	}

	// Call GitHub API to update
	_, _, err := gh.PullRequests.Edit(ctx, owner, repo, prNumber, &github.PullRequest{
		Body: github.Ptr(newDescription),
	})
	if err != nil {
		log.With("error", err).Error("Failed to update PR description")
		result := claudetool.ErrorWithContext(err, map[string]any{"new_description_length": len(newDescription)})
		tc.Complete(result, err)
		return result
	}

	result := map[string]any{
		"success":         true,
		"message":         "PR description updated successfully",
		"description_len": len(newDescription),
	}
	tc.Complete(result, nil)
	return result
}
