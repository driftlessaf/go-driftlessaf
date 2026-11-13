/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/commitscope"
	"github.com/google/go-github/v88/github"
)

const workflowEditLabel = "test-bot/allow-workflow-edits"

func TestWithWorkflowEditLabel(t *testing.T) {
	r := &Reconciler[*testRequest, *testResult, testCallbacks]{}
	WithWorkflowEditLabel[*testRequest, *testResult, testCallbacks](workflowEditLabel)(r)
	if r.workflowEditLabel != workflowEditLabel {
		t.Errorf("reconciler.workflowEditLabel = %q, want = %q", r.workflowEditLabel, workflowEditLabel)
	}
}

func TestDeclareWorkflowEdit(t *testing.T) {
	tests := []struct {
		name  string
		label string
		issue *github.Issue
		want  bool
	}{{
		name:  "no label configured ignores the issue's labels",
		label: "",
		issue: &github.Issue{Labels: []*github.Label{{Name: new(workflowEditLabel)}}},
		want:  false,
	}, {
		name:  "label configured but absent",
		label: workflowEditLabel,
		issue: &github.Issue{Labels: []*github.Label{{Name: new("test-bot/managed")}}},
		want:  false,
	}, {
		name:  "label configured and present",
		label: workflowEditLabel,
		issue: &github.Issue{Labels: []*github.Label{{Name: new("test-bot/managed")}, {Name: new(workflowEditLabel)}}},
		want:  true,
	}, {
		name:  "a different identity's label does not count",
		label: workflowEditLabel,
		issue: &github.Issue{Labels: []*github.Label{{Name: new("other-bot/allow-workflow-edits")}}},
		want:  false,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler[*testRequest, *testResult, testCallbacks]{workflowEditLabel: tt.label}
			scope := commitscope.NewScope()
			r.declareWorkflowEdit(tt.issue, scope)
			if got := scope.WorkflowEditDeclared(); got != tt.want {
				t.Errorf("WorkflowEditDeclared() = %v, want = %v", got, tt.want)
			}
			// A run without the guard carries no scope; declaring is a no-op.
			r.declareWorkflowEdit(tt.issue, nil)
		})
	}
}

// writesWorkflow is an agent that edits a workflow file through the managed
// edit-tool callbacks, so the commit scope records the path as intended.
func writesWorkflow(ctx context.Context) error {
	wt, ok := clonemanager.WorktreeFromContext(ctx)
	if !ok {
		return errors.New("missing leased worktree")
	}
	return clonemanager.WorktreeCallbacks(wt).WriteFile(ctx, ".github/workflows/ci.yaml", "on: pull_request\n", 0o644)
}

// TestReconcileIssueWorkflowEditLabel drives the commit guard's workflow rule
// through a reconcile. The guard denies workflow edits; the agent's only change
// is a workflow file. Without the label the run commits nothing, opens no PR,
// and the give-up note on the issue names the dropped path, so the filer learns
// that the label is what is missing. With the label the run commits the
// workflow and opens the PR.
func TestReconcileIssueWorkflowEditLabel(t *testing.T) {
	tests := []struct {
		name      string
		labels    []string
		unlabel   bool // the agent's run removes the workflow-edit label
		wantPRs   int
		wantNotes int
	}{{
		name:      "label absent drops the workflow edit",
		labels:    []string{requiredLabel},
		wantPRs:   0,
		wantNotes: 1,
	}, {
		name:      "label present commits the workflow edit",
		labels:    []string{requiredLabel, workflowEditLabel},
		wantPRs:   1,
		wantNotes: 0,
	}, {
		// The declaration is read from the issue as refreshed just before the
		// commit, so a label present when the run started but gone by then
		// does not lift the rule.
		name:      "label removed while the agent ran drops the workflow edit",
		labels:    []string{requiredLabel, workflowEditLabel},
		unlabel:   true,
		wantPRs:   0,
		wantNotes: 1,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Isolate the developer's git configuration so a global excludesfile
			// cannot hide the untracked workflow from the guard's status read.
			empty := t.TempDir()
			t.Setenv("HOME", empty)
			t.Setenv("XDG_CONFIG_HOME", empty)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

			h := newIssueHarness(t)
			h.setLabels(tt.labels...)
			dl := commitscope.DefaultDenylist()
			dl.DenyWorkflowEdits = true
			h.cloneOpts = []clonemanager.Option{clonemanager.WithCommitScopeGuard(dl)}

			agent := writesWorkflow
			if tt.unlabel {
				agent = func(ctx context.Context) error {
					if err := writesWorkflow(ctx); err != nil {
						return err
					}
					h.setLabels(requiredLabel)
					return nil
				}
			}
			h.reconcile(t, agent,
				withGiveUp(),
				WithWorkflowEditLabel[*testRequest, *publishResult, struct{}](workflowEditLabel),
			)

			if h.agentCalls != 1 {
				t.Errorf("agent calls: got = %d, want = 1", h.agentCalls)
			}
			if h.prCreates != tt.wantPRs {
				t.Errorf("PRs created: got = %d, want = %d", h.prCreates, tt.wantPRs)
			}
			notes := h.notes()
			if len(notes) != tt.wantNotes {
				t.Fatalf("bot's notes: got = %d (%q), want = %d", len(notes), notes, tt.wantNotes)
			}
			if tt.wantNotes > 0 && !strings.Contains(notes[0], ".github/workflows/ci.yaml") {
				t.Errorf("give-up note: got = %q, want it to name the dropped workflow path", notes[0])
			}
		})
	}
}

// writesWorkflowAndNote is an agent that edits a workflow file and an ordinary
// file through the managed edit-tool callbacks, so the guard drops the first
// and commits the second.
func writesWorkflowAndNote(ctx context.Context) error {
	if err := writesWorkflow(ctx); err != nil {
		return err
	}
	wt, ok := clonemanager.WorktreeFromContext(ctx)
	if !ok {
		return errors.New("missing leased worktree")
	}
	return clonemanager.WorktreeCallbacks(wt).WriteFile(ctx, "docs/note.md", "A note.\n", 0o644)
}

// TestReconcileIssuePRBodyCarriesDroppedWorkflow proves the guard's drops reach
// the opened PR: when the agent's change is partly committed and partly dropped,
// the PR opens for the committed part and its body template sees the dropped
// workflow path through PRData.Dropped, so a consumer can name it there.
func TestReconcileIssuePRBodyCarriesDroppedWorkflow(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("XDG_CONFIG_HOME", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	h := newIssueHarness(t)
	h.setLabels(requiredLabel)
	h.prBody = "Resolve #123.{{range .Dropped}}\nLeft out: {{.Path}} ({{.Reason}}){{end}}"
	dl := commitscope.DefaultDenylist()
	dl.DenyWorkflowEdits = true
	h.cloneOpts = []clonemanager.Option{clonemanager.WithCommitScopeGuard(dl)}

	h.reconcile(t, writesWorkflowAndNote,
		withGiveUp(),
		WithWorkflowEditLabel[*testRequest, *publishResult, struct{}](workflowEditLabel),
	)

	if h.prCreates != 1 {
		t.Fatalf("PRs created: got = %d, want = 1", h.prCreates)
	}
	if got := h.prBodies[0]; !strings.Contains(got, "Left out: .github/workflows/ci.yaml (") {
		t.Errorf("PR body: got = %q, want it to name the dropped workflow path", got)
	}
	if notes := h.notes(); len(notes) != 0 {
		t.Errorf("bot's notes: got = %d (%q), want = 0 since the PR carries the rest of the change", len(notes), notes)
	}
}
