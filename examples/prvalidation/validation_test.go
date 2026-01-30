/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package prvalidation

import (
	"strings"
	"testing"
)

func TestConventionalCommitRegex(t *testing.T) {
	valid := []string{
		"feat: add feature", "fix: bug", "docs: update", "refactor(scope): change",
		"feat(auth): add OAuth", "chore: cleanup", "test: add tests", "ci: workflow",
	}
	invalid := []string{
		"feat add feature", "feat:no space", "FEAT: uppercase", "feature: wrong type",
		"feat:", "random text", "", "   ",
	}

	for _, title := range valid {
		if !ConventionalCommitRegex.MatchString(title) {
			t.Errorf("expected %q to match", title)
		}
	}
	for _, title := range invalid {
		if ConventionalCommitRegex.MatchString(title) {
			t.Errorf("expected %q to NOT match", title)
		}
	}
}

func TestValidatePR(t *testing.T) {
	tests := []struct {
		name           string
		title          string
		body           string
		wantTitleValid bool
		wantDescValid  bool
		wantIssueCount int
	}{{
		name:           "valid title and description",
		title:          "feat: add feature",
		body:           "This is a valid description.",
		wantTitleValid: true,
		wantDescValid:  true,
		wantIssueCount: 0,
	}, {
		name:           "valid title with scope",
		title:          "fix(api): bug fix",
		body:           "A description with enough chars",
		wantTitleValid: true,
		wantDescValid:  true,
		wantIssueCount: 0,
	}, {
		name:           "invalid title format",
		title:          "Bad title",
		body:           "This is a valid description.",
		wantTitleValid: false,
		wantDescValid:  true,
		wantIssueCount: 1,
	}, {
		name:           "empty description",
		title:          "feat: good title",
		body:           "",
		wantTitleValid: true,
		wantDescValid:  false,
		wantIssueCount: 1,
	}, {
		name:           "short description",
		title:          "feat: good title",
		body:           "too short",
		wantTitleValid: true,
		wantDescValid:  false,
		wantIssueCount: 1,
	}, {
		name:           "both invalid",
		title:          "Bad title",
		body:           "short",
		wantTitleValid: false,
		wantDescValid:  false,
		wantIssueCount: 2,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			titleValid, descValid, issues := ValidatePR(tt.title, tt.body)

			if titleValid != tt.wantTitleValid {
				t.Errorf("titleValid: got %v, want %v", titleValid, tt.wantTitleValid)
			}
			if descValid != tt.wantDescValid {
				t.Errorf("descValid: got %v, want %v", descValid, tt.wantDescValid)
			}
			if len(issues) != tt.wantIssueCount {
				t.Errorf("issue count: got %d, want %d", len(issues), tt.wantIssueCount)
			}
		})
	}
}

func TestComputeGeneration(t *testing.T) {
	// Same inputs should produce same output (deterministic)
	gen1 := ComputeGeneration("abc123", "feat: title", "body text")
	gen2 := ComputeGeneration("abc123", "feat: title", "body text")
	if gen1 != gen2 {
		t.Errorf("same inputs should produce same generation: got %s and %s", gen1, gen2)
	}

	// Different SHA should produce different generation
	gen3 := ComputeGeneration("def456", "feat: title", "body text")
	if gen1 == gen3 {
		t.Error("different SHA should produce different generation")
	}

	// Different title should produce different generation
	gen4 := ComputeGeneration("abc123", "fix: different", "body text")
	if gen1 == gen4 {
		t.Error("different title should produce different generation")
	}

	// Different body should produce different generation
	gen5 := ComputeGeneration("abc123", "feat: title", "different body")
	if gen1 == gen5 {
		t.Error("different body should produce different generation")
	}

	// Generation should be a valid hex string (64 chars for SHA256)
	if len(gen1) != 64 {
		t.Errorf("generation should be 64 hex chars, got %d: %s", len(gen1), gen1)
	}
}

func TestDetailsMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		details  Details
		contains []string
	}{{
		name: "all valid",
		details: Details{
			TitleValid:       true,
			DescriptionValid: true,
			Issues:           nil,
		},
		contains: []string{"✅ Valid", "PR Validation Report"},
	}, {
		name: "with issues",
		details: Details{
			TitleValid:       false,
			DescriptionValid: true,
			Issues:           []string{"Title issue"},
		},
		contains: []string{"❌ Invalid", "### Issues", "Title issue"},
	}, {
		name: "with agent activity",
		details: Details{
			TitleValid:       true,
			DescriptionValid: true,
			AgentEnabled:     true,
			FixesApplied:     []string{"Updated title to conventional commit format"},
			AgentReasoning:   "The title was missing the type prefix",
			FixAttempts:      1,
		},
		contains: []string{"Agent Activity", "Updated title", "type prefix", "**Fix Attempts:** 1"},
	}, {
		name: "agent enabled but no fixes",
		details: Details{
			TitleValid:       false,
			DescriptionValid: false,
			Issues:           []string{"Title invalid", "Description too short"},
			AgentEnabled:     true,
			FixAttempts:      2,
			AgentReasoning:   "Could not determine appropriate fixes",
		},
		contains: []string{"Agent Activity", "Could not determine", "**Fix Attempts:** 2"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := tt.details.Markdown()
			for _, s := range tt.contains {
				if !strings.Contains(md, s) {
					t.Errorf("Markdown() missing %q in output:\n%s", s, md)
				}
			}
		})
	}
}
