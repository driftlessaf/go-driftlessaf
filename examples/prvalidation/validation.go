/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package prvalidation provides shared PR validation logic for GitHub PR examples.
package prvalidation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// ConventionalPrefixes lists valid conventional commit types.
var ConventionalPrefixes = []string{
	"feat", "fix", "docs", "style", "refactor",
	"perf", "test", "build", "ci", "chore", "revert",
}

// ConventionalCommitRegex matches titles like "feat: add new feature" or "fix(scope): bug fix"
var ConventionalCommitRegex = regexp.MustCompile(`^(` + strings.Join(ConventionalPrefixes, "|") + `)(\(.+\))?:\s+.+`)

// Details holds validation-specific state for the status manager.
// This is persisted in the check run and can be retrieved on subsequent reconciliations.
type Details struct {
	// Generation is a hash of SHA + title + body for idempotency.
	Generation       string   `json:"generation"`
	TitleValid       bool     `json:"titleValid"`
	DescriptionValid bool     `json:"descriptionValid"`
	Issues           []string `json:"issues,omitempty"`

	// Agent fields (optional, used by github-pr-autofix)
	AgentEnabled   bool     `json:"agentEnabled,omitempty"`
	FixesApplied   []string `json:"fixesApplied,omitempty"`
	AgentReasoning string   `json:"agentReasoning,omitempty"`
	FixAttempts    int      `json:"fixAttempts,omitempty"`
	ModelUsed      string   `json:"modelUsed,omitempty"`
}

// Markdown renders the validation details as markdown for the check run output.
func (d Details) Markdown() string {
	var sb strings.Builder
	sb.WriteString("## PR Validation Report\n\n")
	sb.WriteString("| Check | Status |\n")
	sb.WriteString("|-------|--------|\n")

	titleStatus := "❌ Invalid"
	if d.TitleValid {
		titleStatus = "✅ Valid"
	}
	sb.WriteString(fmt.Sprintf("| Title (conventional commit) | %s |\n", titleStatus))

	descStatus := "❌ Invalid"
	if d.DescriptionValid {
		descStatus = "✅ Valid"
	}
	sb.WriteString(fmt.Sprintf("| Description | %s |\n", descStatus))

	if len(d.Issues) > 0 {
		sb.WriteString("\n### Issues\n\n")
		for _, issue := range d.Issues {
			sb.WriteString(issue)
			sb.WriteString("\n\n---\n\n")
		}
	}

	if d.AgentEnabled {
		sb.WriteString("\n### Agent Activity\n\n")
		if d.ModelUsed != "" {
			sb.WriteString(fmt.Sprintf("**Model:** `%s`\n\n", d.ModelUsed))
		}
		if len(d.FixesApplied) > 0 {
			sb.WriteString("**Fixes Applied:**\n")
			for _, fix := range d.FixesApplied {
				sb.WriteString(fmt.Sprintf("- %s\n", fix))
			}
			sb.WriteString("\n")
		}
		if d.AgentReasoning != "" {
			sb.WriteString(fmt.Sprintf("**Agent Reasoning:** %s\n\n", d.AgentReasoning))
		}
		if d.FixAttempts > 0 {
			sb.WriteString(fmt.Sprintf("**Fix Attempts:** %d\n", d.FixAttempts))
		}
	}

	return sb.String()
}

// ComputeGeneration creates a unique key from SHA, title, and body.
// This ensures idempotency is based on the full PR state, not just the commit.
func ComputeGeneration(sha, title, body string) string {
	h := sha256.New()
	h.Write([]byte(sha))
	h.Write([]byte(title))
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// ValidatePR checks the PR title and description against conventions.
// Returns whether title is valid, description is valid, and a list of issues.
func ValidatePR(title, body string) (titleValid, descValid bool, issues []string) {
	// Validate title follows conventional commit format
	titleValid = ConventionalCommitRegex.MatchString(title)
	if !titleValid {
		issues = append(issues, fmt.Sprintf(
			"**Title** does not follow [conventional commit](https://www.conventionalcommits.org/) format.\n"+
				"  - Expected: `<type>: <description>` or `<type>(scope): <description>`\n"+
				"  - Valid types: `%s`\n"+
				"  - Got: `%s`",
			strings.Join(ConventionalPrefixes, "`, `"),
			title,
		))
	}

	// Validate description is not empty
	trimmedBody := strings.TrimSpace(body)
	switch {
	case trimmedBody == "":
		issues = append(issues, "**Description** is empty. Please add a description explaining the changes.")
	case len(trimmedBody) < 20:
		issues = append(issues, "**Description** is too short. Please provide more context about the changes.")
	default:
		descValid = true
	}

	return titleValid, descValid, issues
}
