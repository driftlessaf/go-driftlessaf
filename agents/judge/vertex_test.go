//go:build withauth

/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"testing"
)

func TestNewVertex(t *testing.T) {
	ctx := context.Background()
	projectID := "test-project"
	region := "us-central1"

	tests := []struct {
		name      string
		modelName string
		wantError bool
	}{{
		name:      "claude model",
		modelName: "claude-sonnet-4@20250514",
		wantError: false,
	}, {
		name:      "gemini model",
		modelName: "gemini-2.5-flash",
		wantError: false,
	}, {
		name:      "unsupported model",
		modelName: "gpt-4",
		wantError: true,
	}, {
		name:      "empty model",
		modelName: "",
		wantError: true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			judgeInstance, err := NewVertex(ctx, projectID, region, tc.modelName)

			if tc.wantError {
				if err == nil {
					t.Errorf("NewVertex() expected error for model %s, got none", tc.modelName)
				}
				if judgeInstance != nil {
					t.Errorf("NewVertex() expected nil judge for error case, got %T", judgeInstance)
				}
			} else {
				if err != nil {
					t.Errorf("NewVertex() unexpected error for model %s: %v", tc.modelName, err)
				}
				if judgeInstance == nil {
					t.Errorf("NewVertex() expected judge for model %s, got nil", tc.modelName)
				}
			}
		})
	}
}
