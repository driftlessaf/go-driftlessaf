/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"testing"
)

// fakeMarkerCommenter records GiveUpComment's calls into the marker primitives.
type fakeMarkerCommenter struct {
	upserted map[string]string
	deleted  []string
}

func newFakeMarkerCommenter() *fakeMarkerCommenter {
	return &fakeMarkerCommenter{upserted: map[string]string{}}
}

func (c *fakeMarkerCommenter) UpsertMarkerComment(_ context.Context, marker, body string) error {
	c.upserted[marker] = body
	return nil
}

func (c *fakeMarkerCommenter) DeleteMarkerComment(_ context.Context, marker string) error {
	c.deleted = append(c.deleted, marker)
	return nil
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

func TestGiveUpCommentClear(t *testing.T) {
	c := newFakeMarkerCommenter()
	testGiveUp().Clear(t.Context(), c)

	if len(c.deleted) != 1 || c.deleted[0] != giveUpMarker {
		t.Errorf("deleted: got = %v, want = [%q]", c.deleted, giveUpMarker)
	}
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
}

// TestGiveUpCommentNilReceiver verifies a nil *GiveUpComment is a safe no-op, so
// reconcilers can call its methods unconditionally.
func TestGiveUpCommentNilReceiver(t *testing.T) {
	var g *GiveUpComment
	c := newFakeMarkerCommenter()

	g.SurfaceResult(t.Context(), c, explainable{reason: "x"})
	g.Surface(t.Context(), c, "x")
	g.Clear(t.Context(), c)

	if len(c.upserted) != 0 || len(c.deleted) != 0 {
		t.Errorf("nil receiver mutated state: upserted = %v, deleted = %v", c.upserted, c.deleted)
	}
}
