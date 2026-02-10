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

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"chainguard.dev/driftlessaf/examples/prvalidation"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v75/github"
	"google.golang.org/genai"
)

// PRTools contains callback functions for PR operations.
// Following the WorktreeCallbacks pattern, this uses func fields
// to enable testability and abstraction from the GitHub client.
type PRTools struct {
	UpdateTitle       func(ctx context.Context, newTitle string) error
	UpdateDescription func(ctx context.Context, newDescription string) error
}

// NewPRTools constructs PRTools with GitHub API closures.
func NewPRTools(gh *github.Client, owner, repo string, prNumber int) PRTools {
	return PRTools{
		UpdateTitle: func(ctx context.Context, newTitle string) error {
			_, _, err := gh.PullRequests.Edit(ctx, owner, repo, prNumber, &github.PullRequest{
				Title: github.Ptr(newTitle),
			})
			return err
		},
		UpdateDescription: func(ctx context.Context, newDescription string) error {
			_, _, err := gh.PullRequests.Edit(ctx, owner, repo, prNumber, &github.PullRequest{
				Body: github.Ptr(newDescription),
			})
			return err
		},
	}
}

// prToolsProvider implements toolcall.ToolProvider[*PRFixResult, PRTools].
type prToolsProvider struct{}

var _ toolcall.ToolProvider[*PRFixResult, PRTools] = prToolsProvider{}

// NewPRToolsProvider creates a new ToolProvider for PR tools.
func NewPRToolsProvider() toolcall.ToolProvider[*PRFixResult, PRTools] {
	return prToolsProvider{}
}

const (
	updateTitleDescription = `Update the PR title to fix validation issues.

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
Max length: 72 characters`

	updateDescriptionDescription = `Update the PR description/body to fix validation issues.
The description should be meaningful and at least 20 characters.
Preserve any existing content that is useful.`
)

func (prToolsProvider) ClaudeTools(cb PRTools) map[string]claudetool.Metadata[*PRFixResult] {
	return map[string]claudetool.Metadata[*PRFixResult]{
		"update_pr_title": {
			Definition: anthropic.ToolParam{
				Name:        "update_pr_title",
				Description: anthropic.String(updateTitleDescription),
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
			Handler: claudeUpdateTitleHandler(cb.UpdateTitle),
		},
		"update_pr_description": {
			Definition: anthropic.ToolParam{
				Name:        "update_pr_description",
				Description: anthropic.String(updateDescriptionDescription),
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
			Handler: claudeUpdateDescriptionHandler(cb.UpdateDescription),
		},
	}
}

func (prToolsProvider) GoogleTools(cb PRTools) map[string]googletool.Metadata[*PRFixResult] {
	return map[string]googletool.Metadata[*PRFixResult]{
		"update_pr_title": {
			Definition: &genai.FunctionDeclaration{
				Name:        "update_pr_title",
				Description: updateTitleDescription,
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"reasoning": {Type: "string", Description: "Explain why this title change fixes the issue"},
						"new_title": {Type: "string", Description: "The new PR title in conventional commit format"},
					},
					Required: []string{"reasoning", "new_title"},
				},
			},
			Handler: googleUpdateTitleHandler(cb.UpdateTitle),
		},
		"update_pr_description": {
			Definition: &genai.FunctionDeclaration{
				Name:        "update_pr_description",
				Description: updateDescriptionDescription,
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"reasoning":       {Type: "string", Description: "Explain why this description change fixes the issue"},
						"new_description": {Type: "string", Description: "The new PR description"},
					},
					Required: []string{"reasoning", "new_description"},
				},
			},
			Handler: googleUpdateDescriptionHandler(cb.UpdateDescription),
		},
	}
}

// Validation helpers

func validateTitle(title string) error {
	if len(title) > 72 {
		return fmt.Errorf("title must be under 72 characters, got %d", len(title))
	}
	if !prvalidation.ConventionalCommitRegex.MatchString(title) {
		return fmt.Errorf("title does not match conventional commit format: %s", title)
	}
	return nil
}

func validateDescription(description string) error {
	if len(strings.TrimSpace(description)) < 20 {
		return fmt.Errorf("description must be at least 20 characters, got %d", len(strings.TrimSpace(description)))
	}
	return nil
}

// Claude handler factories

func claudeUpdateTitleHandler(updateFn func(context.Context, string) error) func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[*PRFixResult], **PRFixResult) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[*PRFixResult], _ **PRFixResult) map[string]any {
		log := clog.FromContext(ctx)

		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("parameter error"))
			return errResp
		}

		reasoning, errResp := claudetool.Param[string](params, "reasoning")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		newTitle, errResp := claudetool.Param[string](params, "new_title")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing new_title parameter"))
			return errResp
		}

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, params.RawInputs())

		if err := validateTitle(newTitle); err != nil {
			result := claudetool.Error("%s", err)
			tc.Complete(result, err)
			return result
		}

		if err := updateFn(ctx, newTitle); err != nil {
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
}

func claudeUpdateDescriptionHandler(updateFn func(context.Context, string) error) func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[*PRFixResult], **PRFixResult) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[*PRFixResult], _ **PRFixResult) map[string]any {
		log := clog.FromContext(ctx)

		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("parameter error"))
			return errResp
		}

		reasoning, errResp := claudetool.Param[string](params, "reasoning")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		newDescription, errResp := claudetool.Param[string](params, "new_description")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing new_description parameter"))
			return errResp
		}

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, params.RawInputs())

		if err := validateDescription(newDescription); err != nil {
			result := claudetool.Error("%s", err)
			tc.Complete(result, err)
			return result
		}

		if err := updateFn(ctx, newDescription); err != nil {
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
}

// Google handler factories

func googleUpdateTitleHandler(updateFn func(context.Context, string) error) func(context.Context, *genai.FunctionCall, *agenttrace.Trace[*PRFixResult], **PRFixResult) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[*PRFixResult], _ **PRFixResult) *genai.FunctionResponse {
		log := clog.FromContext(ctx)

		reasoning, errResp := googletool.Param[string](call, "reasoning")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		newTitle, errResp := googletool.Param[string](call, "new_title")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing new_title parameter"))
			return errResp
		}

		tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"new_title": newTitle})

		if err := validateTitle(newTitle); err != nil {
			resp := googletool.Error(call, "%s", err)
			tc.Complete(resp.Response, err)
			return resp
		}

		if err := updateFn(ctx, newTitle); err != nil {
			log.With("error", err).Error("Failed to update PR title")
			resp := googletool.ErrorWithContext(call, err, map[string]any{"new_title": newTitle})
			tc.Complete(resp.Response, err)
			return resp
		}

		result := map[string]any{
			"success":   true,
			"message":   "PR title updated successfully",
			"new_title": newTitle,
		}
		tc.Complete(result, nil)
		return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: result}
	}
}

func googleUpdateDescriptionHandler(updateFn func(context.Context, string) error) func(context.Context, *genai.FunctionCall, *agenttrace.Trace[*PRFixResult], **PRFixResult) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[*PRFixResult], _ **PRFixResult) *genai.FunctionResponse {
		log := clog.FromContext(ctx)

		reasoning, errResp := googletool.Param[string](call, "reasoning")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		newDescription, errResp := googletool.Param[string](call, "new_description")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing new_description parameter"))
			return errResp
		}

		tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"new_description_length": len(newDescription)})

		if err := validateDescription(newDescription); err != nil {
			resp := googletool.Error(call, "%s", err)
			tc.Complete(resp.Response, err)
			return resp
		}

		if err := updateFn(ctx, newDescription); err != nil {
			log.With("error", err).Error("Failed to update PR description")
			resp := googletool.ErrorWithContext(call, err, map[string]any{"new_description_length": len(newDescription)})
			tc.Complete(resp.Response, err)
			return resp
		}

		result := map[string]any{
			"success":         true,
			"message":         "PR description updated successfully",
			"description_len": len(newDescription),
		}
		tc.Complete(result, nil)
		return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: result}
	}
}
