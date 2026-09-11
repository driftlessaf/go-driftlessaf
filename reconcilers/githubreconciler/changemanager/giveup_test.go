/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"errors"
	"testing"
)

// fakeMarkerCommenter records GiveUpComment's calls into the marker primitives,
// on both the PR side (markerCommenter) and the issue side
// (issueMarkerCommenter).
type fakeMarkerCommenter struct {
	upserted   map[string]string
	deleted    []string
	applyCalls int
	clearCalls int
	upsertErr  error
	clearErr   error

	// issueUpserted records issue-side upserts as marker -> revision + "\n" + body.
	issueUpserted  map[string]string
	issueDeleted   []string
	issueChanged   bool // what UpsertIssueMarkerCommentForRevision reports
	issueUpsertErr error
	issueDeleteErr error
}

func newFakeMarkerCommenter() *fakeMarkerCommenter {
	return &fakeMarkerCommenter{upserted: map[string]string{}, issueUpserted: map[string]string{}, issueChanged: true}
}

func (c *fakeMarkerCommenter) UpsertIssueMarkerCommentForRevision(_ context.Context, marker, revision, body string) (bool, error) {
	if c.issueUpsertErr != nil {
		return false, c.issueUpsertErr
	}
	c.issueUpserted[marker] = revision + "\n" + body
	return c.issueChanged, nil
}

func (c *fakeMarkerCommenter) DeleteIssueMarkerComment(_ context.Context, marker string) error {
	if c.issueDeleteErr != nil {
		return c.issueDeleteErr
	}
	c.issueDeleted = append(c.issueDeleted, marker)
	return nil
}

func (c *fakeMarkerCommenter) UpsertMarkerComment(_ context.Context, marker, body string) error {
	if c.upsertErr != nil {
		return c.upsertErr
	}
	c.upserted[marker] = body
	return nil
}

func (c *fakeMarkerCommenter) DeleteMarkerComment(_ context.Context, marker string) error {
	c.deleted = append(c.deleted, marker)
	return nil
}

func (c *fakeMarkerCommenter) ApplyGaveUp(_ context.Context) (string, error) {
	c.applyCalls++
	return "", nil
}

func (c *fakeMarkerCommenter) ClearGaveUp(_ context.Context) (string, error) {
	c.clearCalls++
	if c.clearErr != nil {
		return "", c.clearErr
	}
	return "", nil
}

// explainable implements Explainer.
type explainable struct{ reason string }

func (e explainable) GetNoChangeExplanation() string { return e.reason }

// pointerExplainable implements Explainer with a pointer receiver, so a typed
// nil would panic if dereferenced.
type pointerExplainable struct{ reason string }

func (e *pointerExplainable) GetNoChangeExplanation() string { return e.reason }

const giveUpMarker = "<!--test:no-changes-->"

func testGiveUp() *GiveUpComment {
	return &GiveUpComment{
		Marker: giveUpMarker,
		Render: func(explanation string) string { return "render: " + explanation },
	}
}

func TestGiveUpCommentSurfaceResult(t *testing.T) {
	tests := []struct {
		name   string
		result any
		want   string // empty means nothing upserted
	}{{
		name:   "explainer with reason",
		result: explainable{reason: "blocked on foo"},
		want:   "render: blocked on foo",
	}, {
		name:   "explainer empty reason",
		result: explainable{reason: ""},
		want:   "",
	}, {
		name:   "not an explainer",
		result: struct{ commitMsg string }{commitMsg: "x"},
		want:   "",
	}, {
		name:   "typed-nil explainer",
		result: (*pointerExplainable)(nil),
		want:   "",
	}, {
		name:   "pointer explainer with reason",
		result: &pointerExplainable{reason: "blocked on bar"},
		want:   "render: blocked on bar",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeMarkerCommenter()
			testGiveUp().SurfaceResult(t.Context(), c, tt.result)

			wantApplyCalls := 0
			if tt.want != "" {
				wantApplyCalls = 1
			}
			if c.applyCalls != wantApplyCalls {
				t.Errorf("applyCalls: got = %d, want = %d", c.applyCalls, wantApplyCalls)
			}

			if tt.want == "" {
				if len(c.upserted) != 0 {
					t.Errorf("upserted: got = %v, want none", c.upserted)
				}
				return
			}
			if got := c.upserted[giveUpMarker]; got != tt.want {
				t.Errorf("upserted body: got = %q, want = %q", got, tt.want)
			}
		})
	}
}

// TestSurfaceSkipsLabelOnUpsertError verifies the give-up label is not
// applied when the underlying comment upsert fails, so a labeled PR always
// has a matching give-up comment.
func TestSurfaceSkipsLabelOnUpsertError(t *testing.T) {
	c := newFakeMarkerCommenter()
	c.upsertErr = errors.New("upsert failed")
	testGiveUp().Surface(t.Context(), c, "blocked on foo")

	if c.applyCalls != 0 {
		t.Errorf("applyCalls after upsert error: got = %d, want = 0", c.applyCalls)
	}
}

func TestGiveUpCommentClear(t *testing.T) {
	c := newFakeMarkerCommenter()
	testGiveUp().Clear(t.Context(), c)

	if len(c.deleted) != 1 || c.deleted[0] != giveUpMarker {
		t.Errorf("deleted: got = %v, want = [%q]", c.deleted, giveUpMarker)
	}
	if c.clearCalls != 1 {
		t.Errorf("clearCalls: got = %d, want = 1", c.clearCalls)
	}
}

// TestClearSkipsCommentOnLabelClearError verifies the comment is not deleted
// when label-clear fails, preserving the labeled-PR-has-matching-comment
// invariant. Mirror of TestSurfaceSkipsLabelOnUpsertError.
func TestClearSkipsCommentOnLabelClearError(t *testing.T) {
	c := newFakeMarkerCommenter()
	c.clearErr = errors.New("clear failed")
	testGiveUp().Clear(t.Context(), c)

	if len(c.deleted) != 0 {
		t.Errorf("deleted after clear error: got = %v, want = none", c.deleted)
	}
}

func TestGiveUpCommentSurfaceResultOnIssue(t *testing.T) {
	const revision = "a1b2c3d4"
	tests := []struct {
		name        string
		result      any
		changed     bool // what the fake session reports
		upsertErr   error
		want        string // revision + "\n" + rendered body; empty means nothing upserted
		wantChanged bool
	}{{
		name:        "explainer with reason posts and reports the edge",
		result:      explainable{reason: "already fixed on main"},
		changed:     true,
		want:        revision + "\nrender: already fixed on main",
		wantChanged: true,
	}, {
		name:        "unchanged revision posts nothing new and reports no edge",
		result:      explainable{reason: "already fixed on main"},
		changed:     false,
		want:        revision + "\nrender: already fixed on main",
		wantChanged: false,
	}, {
		name:   "explainer empty reason",
		result: explainable{reason: ""},
	}, {
		name:   "not an explainer",
		result: struct{ commitMsg string }{commitMsg: "x"},
	}, {
		name:   "typed-nil explainer",
		result: (*pointerExplainable)(nil),
	}, {
		name:      "upsert error is swallowed and reports no edge",
		result:    explainable{reason: "already fixed on main"},
		upsertErr: errors.New("boom"),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeMarkerCommenter()
			c.issueChanged = tt.changed
			c.issueUpsertErr = tt.upsertErr
			got := testGiveUp().SurfaceResultOnIssue(t.Context(), c, revision, tt.result)

			if got != tt.wantChanged {
				t.Errorf("SurfaceResultOnIssue: got = %t, want = %t", got, tt.wantChanged)
			}
			if tt.want == "" {
				if len(c.issueUpserted) != 0 {
					t.Errorf("issueUpserted: got = %v, want none", c.issueUpserted)
				}
				return
			}
			if got := c.issueUpserted[giveUpMarker]; got != tt.want {
				t.Errorf("issueUpserted: got = %q, want = %q", got, tt.want)
			}
			// The issue side never touches the PR: no PR comment, no label.
			if len(c.upserted) != 0 || c.applyCalls != 0 {
				t.Errorf("PR side touched from the issue surface: upserted = %v, applyCalls = %d", c.upserted, c.applyCalls)
			}
		})
	}
}

func TestGiveUpCommentClearIssue(t *testing.T) {
	c := newFakeMarkerCommenter()
	testGiveUp().ClearIssue(t.Context(), c)

	if len(c.issueDeleted) != 1 || c.issueDeleted[0] != giveUpMarker {
		t.Errorf("issueDeleted: got = %v, want = [%q]", c.issueDeleted, giveUpMarker)
	}
	// The issue side never touches the PR comment or label.
	if len(c.deleted) != 0 || c.clearCalls != 0 {
		t.Errorf("PR side touched from ClearIssue: deleted = %v, clearCalls = %d", c.deleted, c.clearCalls)
	}

	// A delete failure is logged and swallowed, never panics or propagates.
	c = newFakeMarkerCommenter()
	c.issueDeleteErr = errors.New("boom")
	testGiveUp().ClearIssue(t.Context(), c)
}

// TestGiveUpCommentNilRender verifies Surface is a no-op (rather than panicking)
// when Render is nil, so a misconfigured GiveUpComment cannot crash a reconcile.
func TestGiveUpCommentNilRender(t *testing.T) {
	c := newFakeMarkerCommenter()
	g := &GiveUpComment{Marker: giveUpMarker} // Render left nil
	g.SurfaceResult(t.Context(), c, explainable{reason: "blocked on foo"})

	if len(c.upserted) != 0 {
		t.Errorf("upserted: got = %v, want none when Render is nil", c.upserted)
	}
	if c.applyCalls != 0 {
		t.Errorf("applyCalls: got = %d, want 0 when Render is nil", c.applyCalls)
	}

	if g.SurfaceResultOnIssue(t.Context(), c, "rev", explainable{reason: "blocked on foo"}) {
		t.Error("SurfaceResultOnIssue: got = true, want = false when Render is nil")
	}
	if len(c.issueUpserted) != 0 {
		t.Errorf("issueUpserted: got = %v, want none when Render is nil", c.issueUpserted)
	}
}

// TestGiveUpCommentNilReceiver verifies a nil *GiveUpComment is a safe no-op, so
// reconcilers can call its methods unconditionally.
func TestGiveUpCommentNilReceiver(t *testing.T) {
	var g *GiveUpComment
	c := newFakeMarkerCommenter()

	g.SurfaceResult(t.Context(), c, explainable{reason: "x"})
	g.Surface(t.Context(), c, "x")
	g.Clear(t.Context(), c)
	if g.SurfaceResultOnIssue(t.Context(), c, "rev", explainable{reason: "x"}) {
		t.Error("SurfaceResultOnIssue on nil receiver: got = true, want = false")
	}
	g.ClearIssue(t.Context(), c)

	if len(c.upserted) != 0 || len(c.deleted) != 0 {
		t.Errorf("nil receiver mutated state: upserted = %v, deleted = %v", c.upserted, c.deleted)
	}
	if len(c.issueUpserted) != 0 || len(c.issueDeleted) != 0 {
		t.Errorf("nil receiver mutated issue state: issueUpserted = %v, issueDeleted = %v", c.issueUpserted, c.issueDeleted)
	}
	if c.applyCalls != 0 || c.clearCalls != 0 {
		t.Errorf("nil receiver touched labels: applyCalls = %d, clearCalls = %d", c.applyCalls, c.clearCalls)
	}
}
