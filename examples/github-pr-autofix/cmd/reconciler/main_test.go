/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"strings"
	"testing"
)

// Tests for agent-specific types and functionality.
// Shared validation tests are in prvalidation/validation_test.go

func TestPRContextBind(t *testing.T) {
	ctx := &PRContext{
		Owner:    "testorg",
		Repo:     "testrepo",
		PRNumber: 42,
		Title:    "fix bug",
		Body:     "short",
		Issues:   []string{"Title invalid", "Description too short"},
	}

	// Verify Bind doesn't panic with a valid prompt
	prompt := userPrompt
	bound, err := ctx.Bind(prompt)
	if err != nil {
		t.Fatalf("Bind failed: %v", err)
	}

	// Build the prompt to verify it works
	result, err := bound.Build()
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Verify the bound content includes expected data
	if !strings.Contains(result, "testorg") {
		t.Error("bound prompt should contain owner")
	}
	if !strings.Contains(result, "testrepo") {
		t.Error("bound prompt should contain repo")
	}
	if !strings.Contains(result, "fix bug") {
		t.Error("bound prompt should contain title")
	}
}

func TestPRFixResultFields(t *testing.T) {
	result := &PRFixResult{
		Success:      true,
		FixesApplied: []string{"Updated title", "Updated description"},
		Reasoning:    "Fixed both issues",
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}
	if len(result.FixesApplied) != 2 {
		t.Errorf("expected 2 fixes, got %d", len(result.FixesApplied))
	}
	if result.Reasoning != "Fixed both issues" {
		t.Errorf("unexpected reasoning: %s", result.Reasoning)
	}
}
