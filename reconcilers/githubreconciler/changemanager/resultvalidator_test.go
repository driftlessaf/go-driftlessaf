/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"testing"
)

// contractResult is a minimal agent result implementing the change manager's
// contract, mirroring the shape the reconciler bots use.
type contractResult struct {
	commitMessage   string
	noChangesReason string
}

func (r *contractResult) GetCommitMessage() string       { return r.commitMessage }
func (r *contractResult) GetNoChangeExplanation() string { return r.noChangesReason }

func TestResultValidator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result *contractResult
		wantID string
	}{{
		name:   "nil result",
		wantID: "missing-result",
	}, {
		name: "change with commit message",
		result: &contractResult{
			commitMessage: "widgets: frobnicate the doohickey",
		},
	}, {
		name: "no change with explanation",
		result: &contractResult{
			noChangesReason: "the failure is owned by another reconciler",
		},
	}, {
		name: "whitespace only counts as neither",
		result: &contractResult{
			commitMessage:   "  ",
			noChangesReason: "\n",
		},
		wantID: "no-commit-message-or-reason",
	}, {
		name:   "neither",
		result: &contractResult{},
		wantID: "no-commit-message-or-reason",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			findings, err := ResultValidator[*contractResult]()(t.Context(), tt.result, "reasoning")
			if err != nil {
				t.Fatalf("ResultValidator: got = %v, want = nil", err)
			}
			if tt.wantID == "" {
				if len(findings) != 0 {
					t.Fatalf("findings: got = %v, want = none", findings)
				}
				return
			}
			if len(findings) != 1 || findings[0].Identifier != tt.wantID {
				t.Fatalf("findings: got = %v, want one with identifier %q", findings, tt.wantID)
			}
		})
	}
}
