/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"crypto/sha256"
	"slices"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/google/go-github/v88/github"
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
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
	)

	if rec == nil {
		t.Fatal("New() returned nil")
	}

	// Verify the reconciler was created with expected values
	if rec.identity != "test-identity" {
		t.Errorf("reconciler.identity = %q, wanted = %q", rec.identity, "test-identity")
	}

	if len(rec.prLabels) != 2 {
		t.Errorf("len(reconciler.prLabels) = %d, wanted = 2", len(rec.prLabels))
	}

	if rec.prLabels[0] != "label1" {
		t.Errorf("reconciler.prLabels[0] = %q, wanted = %q", rec.prLabels[0], "label1")
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
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
	)

	if rec == nil {
		t.Fatal("New() returned nil with empty labels")
	}

	if len(rec.prLabels) != 0 {
		t.Errorf("reconciler.prLabels = %v, wanted = empty", rec.prLabels)
	}
}

func TestPRDataFields(t *testing.T) {
	body := "test issue body"
	hash := sha256.Sum256([]byte(body))

	data := PRData[*testRequest]{
		Identity:      "my-bot",
		IssueURL:      "https://github.com/org/repo/issues/123",
		IssueNumber:   123,
		IssueBodyHash: hash,
	}

	if data.Identity != "my-bot" {
		t.Errorf("PRData.Identity = %q, wanted = %q", data.Identity, "my-bot")
	}

	if data.IssueNumber != 123 {
		t.Errorf("PRData.IssueNumber = %d, wanted = 123", data.IssueNumber)
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
		t.Errorf("Result.GetCommitMessage() = %q, wanted = %q", got, "test commit message")
	}
}

func TestResultInterfaceWithEmptyMessage(t *testing.T) {
	result := &testResult{commitMsg: ""}

	var r Result = result

	if got := r.GetCommitMessage(); got != "" {
		t.Errorf("Result.GetCommitMessage() = %q, wanted = empty string", got)
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
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
		WithRequiredLabel[*testRequest, *testResult, testCallbacks]("test-identity/managed"),
	)

	if rec == nil {
		t.Fatal("New() returned nil with WithRequiredLabel option")
	}

	if rec.requiredLabel != "test-identity/managed" {
		t.Errorf("reconciler.requiredLabel = %q, wanted = %q", rec.requiredLabel, "test-identity/managed")
	}
}

func TestWithDraftLabel(t *testing.T) {
	rec := New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil,
		nil,
		nil,
		&fakeAgent{},
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
		WithDraftLabel[*testRequest, *testResult, testCallbacks]("test-identity/draft"),
	)

	if rec == nil {
		t.Fatal("New() returned nil with WithDraftLabel option")
	}
	if rec.draftLabel != "test-identity/draft" {
		t.Errorf("reconciler.draftLabel = %q, wanted = %q", rec.draftLabel, "test-identity/draft")
	}
}

func TestDraftForIssue(t *testing.T) {
	issue := func(labels ...string) *github.Issue {
		i := &github.Issue{}
		for _, l := range labels {
			i.Labels = append(i.Labels, &github.Label{Name: new(l)})
		}
		return i
	}

	tests := []struct {
		name       string
		draftLabel string
		issue      *github.Issue
		want       bool
	}{
		{
			name:       "no label configured is never draft",
			draftLabel: "",
			issue:      issue("test-identity/draft"),
			want:       false,
		},
		{
			name:       "configured label present opens draft",
			draftLabel: "test-identity/draft",
			issue:      issue("test-identity/managed", "test-identity/draft"),
			want:       true,
		},
		{
			name:       "configured label absent is not draft",
			draftLabel: "test-identity/draft",
			issue:      issue("test-identity/managed"),
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler[*testRequest, *testResult, testCallbacks]{draftLabel: tt.draftLabel}
			if got := r.draftForIssue(tt.issue); got != tt.want {
				t.Errorf("draftForIssue() = %v, want = %v", got, tt.want)
			}
		})
	}
}

func TestShouldClearDraftLabel(t *testing.T) {
	issue := func(labels ...string) *github.Issue {
		i := &github.Issue{}
		for _, l := range labels {
			i.Labels = append(i.Labels, &github.Label{Name: new(l)})
		}
		return i
	}

	tests := []struct {
		name       string
		draftLabel string
		issue      *github.Issue
		hasPR      bool
		prIsDraft  bool
		want       bool
	}{
		{
			name:       "promoted PR with label still present clears it",
			draftLabel: "test-identity/draft",
			issue:      issue("test-identity/managed", "test-identity/draft"),
			hasPR:      true,
			prIsDraft:  false,
			want:       true,
		},
		{
			name:       "PR still draft keeps the label",
			draftLabel: "test-identity/draft",
			issue:      issue("test-identity/draft"),
			hasPR:      true,
			prIsDraft:  true,
			want:       false,
		},
		{
			name:       "label already absent is a no-op",
			draftLabel: "test-identity/draft",
			issue:      issue("test-identity/managed"),
			hasPR:      true,
			prIsDraft:  false,
			want:       false,
		},
		{
			name:       "no PR yet is a no-op",
			draftLabel: "test-identity/draft",
			issue:      issue("test-identity/draft"),
			hasPR:      false,
			prIsDraft:  false,
			want:       false,
		},
		{
			name:       "unconfigured label never clears",
			draftLabel: "",
			issue:      issue("test-identity/draft"),
			hasPR:      true,
			prIsDraft:  false,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler[*testRequest, *testResult, testCallbacks]{draftLabel: tt.draftLabel}
			if got := r.shouldClearDraftLabel(tt.issue, tt.hasPR, tt.prIsDraft); got != tt.want {
				t.Errorf("shouldClearDraftLabel() = %v, want = %v", got, tt.want)
			}
		})
	}
}

func TestWithPRLabelsFromResult(t *testing.T) {
	agent := &fakeAgent{}
	fn := func(*testResult) []string { return []string{"team/example"} }

	rec := New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil,
		nil,
		nil,
		agent,
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
		WithPRLabelsFromResult[*testRequest, *testResult, testCallbacks](fn),
	)

	if rec == nil {
		t.Fatal("New() returned nil with WithPRLabelsFromResult option")
	}

	if rec.prLabelsFromResult == nil {
		t.Fatal("reconciler.prLabelsFromResult = nil, wanted the provided function")
	}

	if got := rec.prLabelsFromResult(&testResult{}); len(got) != 1 || got[0] != "team/example" {
		t.Errorf("reconciler.prLabelsFromResult(...) = %v, wanted [team/example]", got)
	}
}

func TestWithReplaceLabelPrefixes(t *testing.T) {
	rec := New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil,
		nil,
		nil,
		&fakeAgent{},
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
		WithReplaceLabelPrefixes[*testRequest, *testResult, testCallbacks]("bot:pkg:", "bot:lang:"),
	)

	want := []string{"bot:pkg:", "bot:lang:"}
	if !slices.Equal(rec.replaceLabelPrefixes, want) {
		t.Errorf("reconciler.replaceLabelPrefixes: got = %v, want = %v", rec.replaceLabelPrefixes, want)
	}
}

func TestWithPRRenderFromResult(t *testing.T) {
	agent := &fakeAgent{}
	fn := func(r *testResult) PRRender {
		return PRRender{Headline: r.commitMsg, VariantSummary: "variant 7"}
	}

	rec := New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil,
		nil,
		nil,
		agent,
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
		WithPRRenderFromResult[*testRequest, *testResult, testCallbacks](fn),
	)

	if rec.prRenderFromResult == nil {
		t.Fatal("reconciler.prRenderFromResult = nil, wanted the provided function")
	}

	got := rec.prRenderFromResult(&testResult{commitMsg: "fix(pkg): rename the field"})
	want := PRRender{Headline: "fix(pkg): rename the field", VariantSummary: "variant 7"}
	if got != want {
		t.Errorf("reconciler.prRenderFromResult(...) = %+v, wanted = %+v", got, want)
	}
}

func TestWithGiveUpComment(t *testing.T) {
	rec := New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil,
		nil,
		nil,
		&fakeAgent{},
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
		WithGiveUpComment[*testRequest, *testResult, testCallbacks]("<!--test:no-changes-->", func(e string) string { return "body: " + e }),
	)

	if rec == nil {
		t.Fatal("New() returned nil with WithGiveUpComment option")
	}
	if rec.giveUp == nil {
		t.Fatal("reconciler.giveUp = nil, want the configured comment")
	}
	if rec.giveUp.Marker != "<!--test:no-changes-->" {
		t.Errorf("reconciler.giveUp.Marker: got = %q, want = %q", rec.giveUp.Marker, "<!--test:no-changes-->")
	}
	if got := rec.giveUp.Render("why"); got != "body: why" {
		t.Errorf("reconciler.giveUp.Render: got = %q, want = %q", got, "body: why")
	}
}

func TestWithStartComment(t *testing.T) {
	rec := New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil,
		nil,
		nil,
		&fakeAgent{},
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
		WithStartComment[*testRequest, *testResult, testCallbacks]("<!--test:start-->", func() string { return "started" }),
	)

	if rec.startComment == nil {
		t.Fatal("reconciler.startComment = nil, want the configured comment")
	}
	if rec.startComment.marker != "<!--test:start-->" {
		t.Errorf("reconciler.startComment.marker: got = %q, want = %q", rec.startComment.marker, "<!--test:start-->")
	}
	if got := rec.startComment.render(); got != "started" {
		t.Errorf("reconciler.startComment.render: got = %q, want = %q", got, "started")
	}
}

// fakeIssueCommenter records the body upserted to the issue, standing in for a
// change Session so the start-comment gate is testable without a reconcile loop.
type fakeIssueCommenter struct {
	upserts []string
}

func (c *fakeIssueCommenter) UpsertIssueMarkerComment(_ context.Context, marker, body string) error {
	c.upserts = append(c.upserts, marker+"\n"+body)
	return nil
}

// TestStartCommentSurface verifies surface posts the marker comment when the
// option is set and no-ops on a nil receiver (the option left unset, as for
// materializer and image-gen). The !state.HasPR() gate that decides whether
// surface is called lives in reconcileIssue.
func TestStartCommentSurface(t *testing.T) {
	tests := []struct {
		name      string
		sc        *startComment
		wantPosts int
	}{{
		name:      "set posts once",
		sc:        &startComment{marker: "<!--test:start-->", render: func() string { return "started" }},
		wantPosts: 1,
	}, {
		name:      "nil receiver no-ops",
		sc:        nil,
		wantPosts: 0,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeIssueCommenter{}
			tt.sc.surface(t.Context(), c)
			if len(c.upserts) != tt.wantPosts {
				t.Errorf("upserts: got = %v, want %d", c.upserts, tt.wantPosts)
			}
		})
	}
}

func TestWithCopyIssueLabels(t *testing.T) {
	rec := New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil,
		nil,
		nil,
		&fakeAgent{},
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
		WithCopyIssueLabels[*testRequest, *testResult, testCallbacks](),
	)

	if rec == nil {
		t.Fatal("New() returned nil with WithCopyIssueLabels option")
	}
	if !rec.copyIssueLabels {
		t.Error("reconciler.copyIssueLabels = false, want true")
	}
}

func TestPRLabelsForIssue(t *testing.T) {
	issue := func(labels ...string) *github.Issue {
		i := &github.Issue{}
		for _, l := range labels {
			i.Labels = append(i.Labels, &github.Label{Name: new(l)})
		}
		return i
	}

	tests := []struct {
		name       string
		prLabels   []string
		copy       bool
		draftLabel string
		issue      *github.Issue
		want       []string
	}{
		{
			name:     "copy off returns fixed labels only",
			prLabels: []string{"test-identity/managed"},
			copy:     false,
			issue:    issue("ai-review", "area/bots"),
			want:     []string{"test-identity/managed"},
		},
		{
			name:     "copy on merges issue labels",
			prLabels: []string{"test-identity/managed"},
			copy:     true,
			issue:    issue("ai-review", "area/bots"),
			want:     []string{"test-identity/managed", "ai-review", "area/bots"},
		},
		{
			name:     "copy on dedupes labels present in both",
			prLabels: []string{"test-identity/managed"},
			copy:     true,
			issue:    issue("test-identity/managed", "ai-review"),
			want:     []string{"test-identity/managed", "ai-review"},
		},
		{
			name:     "copy on with no issue labels returns fixed labels",
			prLabels: []string{"test-identity/managed"},
			copy:     true,
			issue:    issue(),
			want:     []string{"test-identity/managed"},
		},
		{
			name:       "copy on never copies the draft label",
			prLabels:   []string{"test-identity/managed"},
			copy:       true,
			draftLabel: "test-identity/draft",
			issue:      issue("test-identity/draft", "ai-review"),
			want:       []string{"test-identity/managed", "ai-review"},
		},
		{
			name:       "draft label unconfigured copies all issue labels",
			prLabels:   []string{"test-identity/managed"},
			copy:       true,
			draftLabel: "",
			issue:      issue("test-identity/draft", "ai-review"),
			want:       []string{"test-identity/managed", "test-identity/draft", "ai-review"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler[*testRequest, *testResult, testCallbacks]{
				prLabels:        tt.prLabels,
				copyIssueLabels: tt.copy,
				draftLabel:      tt.draftLabel,
			}
			got := r.prLabelsForIssue(tt.issue)
			if !slices.Equal(got, tt.want) {
				t.Errorf("prLabelsForIssue() = %v, want = %v", got, tt.want)
			}
		})
	}
}
