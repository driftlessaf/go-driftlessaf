/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
)

type testRequest struct{}

func (r *testRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p, nil
}

type testResponse struct{}

// testCallbacks is the standard tool composition: Empty -> Worktree -> Finding
type testCallbacks = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]

func TestNewModelSelection(t *testing.T) {
	ctx := context.Background()

	config := Config[*testResponse, testCallbacks]{
		Tools: toolcall.NewFindingToolsProvider[*testResponse, toolcall.WorktreeTools[toolcall.EmptyTools]](
			toolcall.NewWorktreeToolsProvider[*testResponse, toolcall.EmptyTools](
				toolcall.NewEmptyToolsProvider[*testResponse]())),
	}

	tests := []struct {
		name    string
		model   string
		wantErr string
	}{{
		name:    "unsupported model",
		model:   "unknown-model",
		wantErr: "unsupported model",
	}, {
		name:    "empty model",
		model:   "",
		wantErr: "unsupported model",
	}, {
		name:    "partial gemini",
		model:   "gem",
		wantErr: "unsupported model",
	}, {
		name:    "partial claude",
		model:   "cla",
		wantErr: "unsupported model",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New[*testRequest](ctx, "test-project", "us-central1", tt.model, config)
			if err == nil {
				t.Errorf("New() error = nil, wantErr containing %q", tt.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New() error = %v, wantErr containing %q", err, tt.wantErr)
			}
		})
	}
}
