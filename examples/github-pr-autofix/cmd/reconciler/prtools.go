/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"chainguard.dev/driftlessaf/examples/prvalidation"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v75/github"
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

func (prToolsProvider) Tools(cb PRTools) map[string]toolcall.Tool[*PRFixResult] {
	return map[string]toolcall.Tool[*PRFixResult]{
		"update_pr_title":       updatePRTitleTool(cb.UpdateTitle),
		"update_pr_description": updatePRDescriptionTool(cb.UpdateDescription),
	}
}

func updatePRTitleTool(updateFn func(context.Context, string) error) toolcall.Tool[*PRFixResult] {
	return toolcall.Tool[*PRFixResult]{
		Def: toolcall.Definition{
			Name:        "update_pr_title",
			Description: updateTitleDescription,
			Parameters: []toolcall.Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why this title change fixes the issue", Required: true},
				{Name: "new_title", Type: "string", Description: "The new PR title in conventional commit format", Required: true},
			},
		},
		Handler: func(ctx context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[*PRFixResult], _ **PRFixResult) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := toolcall.Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			newTitle, errResp := toolcall.Param[string](call, trace, "new_title")
			if errResp != nil {
				return errResp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"new_title": newTitle})

			if err := validateTitle(newTitle); err != nil {
				result := params.Error("%s", err)
				tc.Complete(result, err)
				return result
			}

			if err := updateFn(ctx, newTitle); err != nil {
				log.With("error", err).Error("Failed to update PR title")
				result := params.ErrorWithContext(err, map[string]any{"new_title": newTitle})
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
		},
	}
}

func updatePRDescriptionTool(updateFn func(context.Context, string) error) toolcall.Tool[*PRFixResult] {
	return toolcall.Tool[*PRFixResult]{
		Def: toolcall.Definition{
			Name:        "update_pr_description",
			Description: updateDescriptionDescription,
			Parameters: []toolcall.Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why this description change fixes the issue", Required: true},
				{Name: "new_description", Type: "string", Description: "The new PR description", Required: true},
			},
		},
		Handler: func(ctx context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[*PRFixResult], _ **PRFixResult) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := toolcall.Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			newDescription, errResp := toolcall.Param[string](call, trace, "new_description")
			if errResp != nil {
				return errResp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"new_description_length": len(newDescription)})

			if err := validateDescription(newDescription); err != nil {
				result := params.Error("%s", err)
				tc.Complete(result, err)
				return result
			}

			if err := updateFn(ctx, newDescription); err != nil {
				log.With("error", err).Error("Failed to update PR description")
				result := params.ErrorWithContext(err, map[string]any{"new_description_length": len(newDescription)})
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
