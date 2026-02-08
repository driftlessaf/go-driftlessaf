/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"crypto/sha256"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v75/github"
)

// testCallbacks is the standard tool composition: Empty -> Worktree -> Finding
type testCallbacks = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]

type testRequest struct{}

func (r *testRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p, nil
}

type testResult struct {
	commitMsg string
}

func (r *testResult) GetCommitMessage() string {
	return r.commitMsg
}

// fakeAgent implements metaagent.Agent for testing
type fakeAgent struct {
	executeResult *testResult
	executeErr    error
}

func (a *fakeAgent) Execute(ctx context.Context, req *testRequest, cb testCallbacks) (*testResult, error) {
	return a.executeResult, a.executeErr
}

func TestNewCreatesReconciler(t *testing.T) {
	agent := &fakeAgent{}

	rec := New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil, // changeManager - not used in this test
		nil, // cloneMeta - not used in this test
		[]string{"label1", "label2"},
		agent,
		func(issue *github.Issue, session *changemanager.Session[PRData]) *testRequest {
			return &testRequest{}
		},
		func(wt *gogit.Worktree, session *changemanager.Session[PRData]) testCallbacks {
			return testCallbacks{}
		},
	)

	if rec == nil {
		t.Fatal("New() returned nil")
	}

	// Verify the reconciler was created with expected values
	if rec.identity != "test-identity" {
		t.Errorf("reconciler.identity = %q, want %q", rec.identity, "test-identity")
	}

	if len(rec.prLabels) != 2 {
		t.Errorf("len(reconciler.prLabels) = %d, want 2", len(rec.prLabels))
	}

	if rec.prLabels[0] != "label1" {
		t.Errorf("reconciler.prLabels[0] = %q, want %q", rec.prLabels[0], "label1")
	}
}

func TestNewWithEmptyLabels(t *testing.T) {
	agent := &fakeAgent{}

	rec := New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil,
		nil,
		nil, // empty labels
		agent,
		func(issue *github.Issue, session *changemanager.Session[PRData]) *testRequest {
			return &testRequest{}
		},
		func(wt *gogit.Worktree, session *changemanager.Session[PRData]) testCallbacks {
			return testCallbacks{}
		},
	)

	if rec == nil {
		t.Fatal("New() returned nil with empty labels")
	}

	if len(rec.prLabels) != 0 {
		t.Errorf("reconciler.prLabels = %v, want empty", rec.prLabels)
	}
}

func TestPRDataFields(t *testing.T) {
	body := "test issue body"
	hash := sha256.Sum256([]byte(body))

	data := PRData{
		Identity:      "my-bot",
		IssueURL:      "https://github.com/org/repo/issues/123",
		IssueNumber:   123,
		IssueBodyHash: hash,
	}

	if data.Identity != "my-bot" {
		t.Errorf("PRData.Identity = %q, want %q", data.Identity, "my-bot")
	}

	if data.IssueNumber != 123 {
		t.Errorf("PRData.IssueNumber = %d, want 123", data.IssueNumber)
	}

	if data.IssueBodyHash != hash {
		t.Error("PRData.IssueBodyHash did not match expected hash")
	}
}

func TestResultInterface(t *testing.T) {
	result := &testResult{commitMsg: "test commit message"}

	// Verify it satisfies the Result interface
	var r Result = result

	if got := r.GetCommitMessage(); got != "test commit message" {
		t.Errorf("Result.GetCommitMessage() = %q, want %q", got, "test commit message")
	}
}

func TestResultInterfaceWithEmptyMessage(t *testing.T) {
	result := &testResult{commitMsg: ""}

	var r Result = result

	if got := r.GetCommitMessage(); got != "" {
		t.Errorf("Result.GetCommitMessage() = %q, want empty string", got)
	}
}

func TestWithRequiredLabel(t *testing.T) {
	agent := &fakeAgent{}

	rec := New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil,
		nil,
		nil,
		agent,
		func(issue *github.Issue, session *changemanager.Session[PRData]) *testRequest {
			return &testRequest{}
		},
		func(wt *gogit.Worktree, session *changemanager.Session[PRData]) testCallbacks {
			return testCallbacks{}
		},
		WithRequiredLabel[*testRequest, *testResult, testCallbacks]("test-identity/managed"),
	)

	if rec == nil {
		t.Fatal("New() returned nil with WithRequiredLabel option")
	}

	if rec.requiredLabel != "test-identity/managed" {
		t.Errorf("reconciler.requiredLabel = %q, want %q", rec.requiredLabel, "test-identity/managed")
	}
}
