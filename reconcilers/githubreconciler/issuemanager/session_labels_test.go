/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package issuemanager

import (
	"testing"
	"text/template"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v88/github"
)

type labelTestData struct{ ID string }

func (d labelTestData) Equal(o labelTestData) bool { return d.ID == o.ID }

// newLabelTestSession builds an IssueSession backed by a real IM so that
// mergeLabels and needsUpdate see the manager's identity and managed labels.
func newLabelTestSession(t *testing.T, opts ...Option[labelTestData]) *IssueSession[labelTestData] {
	t.Helper()

	tmpl := template.Must(template.New("t").Parse("issue {{.ID}}"))
	im, err := New("test-bot", tmpl, tmpl, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &IssueSession[labelTestData]{
		manager:   im,
		pathLabel: "test-bot:some/path",
	}
}

func ghLabels(names ...string) []*github.Label {
	labels := make([]*github.Label, 0, len(names))
	for _, n := range names {
		labels = append(labels, &github.Label{Name: new(n)})
	}
	return labels
}

func TestMergeLabels(t *testing.T) {
	tests := []struct {
		name    string
		managed []string
		current []string
		extras  []string
		want    []string
	}{{
		name:    "human labels preserved",
		current: []string{"test-bot:some/path", "human-added"},
		extras:  []string{"automated"},
		want:    []string{"test-bot:some/path", "human-added", "automated"},
	}, {
		name:    "identity-prefixed labels replaced",
		current: []string{"test-bot:some/path", "test-bot:stale"},
		extras:  []string{"automated"},
		want:    []string{"test-bot:some/path", "automated"},
	}, {
		name:    "stale extra without managed declaration is preserved",
		current: []string{"test-bot:some/path", "prio:high"},
		extras:  []string{"prio:low"},
		want:    []string{"test-bot:some/path", "prio:high", "prio:low"},
	}, {
		name:    "stale managed label is dropped",
		managed: []string{"prio:high", "prio:low"},
		current: []string{"test-bot:some/path", "prio:high"},
		extras:  []string{"prio:low"},
		want:    []string{"test-bot:some/path", "prio:low"},
	}, {
		name:    "managed label still passed as extra is kept",
		managed: []string{"prio:high", "prio:low"},
		current: []string{"test-bot:some/path", "prio:low", "human-added"},
		extras:  []string{"prio:low"},
		want:    []string{"test-bot:some/path", "human-added", "prio:low"},
	}, {
		name: "stale managed label is dropped despite canonical casing",
		// GitHub returns the label entity's canonical casing, which can
		// differ from the declared managed value.
		managed: []string{"prio:high", "prio:low"},
		current: []string{"test-bot:some/path", "Prio:High"},
		extras:  []string{"prio:low"},
		want:    []string{"test-bot:some/path", "prio:low"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []Option[labelTestData]
			if tt.managed != nil {
				opts = append(opts, WithManagedLabels[labelTestData](tt.managed...))
			}
			s := newLabelTestSession(t, opts...)

			got := s.mergeLabels(ghLabels(tt.current...), s.pathLabel, nil, tt.extras)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("mergeLabels mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNeedsUpdateLabelDrift(t *testing.T) {
	data := labelTestData{ID: "001"}

	tests := []struct {
		name    string
		managed []string
		current []string
		extras  []string
		want    bool
	}{{
		name:    "labels converged, no update",
		current: []string{"test-bot:some/path", "automated"},
		extras:  []string{"automated"},
		want:    false,
	}, {
		name:    "missing extra label needs update",
		current: []string{"test-bot:some/path"},
		extras:  []string{"automated"},
		want:    true,
	}, {
		name:    "moved managed label needs update",
		managed: []string{"prio:high", "prio:low"},
		current: []string{"test-bot:some/path", "prio:high"},
		extras:  []string{"prio:low"},
		want:    true,
	}, {
		name:    "human label alone does not trigger update",
		current: []string{"test-bot:some/path", "automated", "human-added"},
		extras:  []string{"automated"},
		want:    false,
	}, {
		name: "casing difference does not trigger update",
		// GitHub label names are case-insensitive and returned in the
		// canonical casing of the label entity.
		current: []string{"test-bot:some/path", "Automated"},
		extras:  []string{"automated"},
		want:    false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []Option[labelTestData]
			if tt.managed != nil {
				opts = append(opts, WithManagedLabels[labelTestData](tt.managed...))
			}
			s := newLabelTestSession(t, opts...)

			existing := &existingIssue[labelTestData]{
				issue: &github.Issue{Labels: ghLabels(tt.current...)},
				data:  &data,
			}
			if got := s.needsUpdate(t.Context(), existing, &data, tt.extras); got != tt.want {
				t.Errorf("needsUpdate() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestNeedsUpdateDataDrift(t *testing.T) {
	s := newLabelTestSession(t)
	existing := &existingIssue[labelTestData]{
		issue: &github.Issue{Labels: ghLabels("test-bot:some/path")},
		data:  &labelTestData{ID: "001"},
	}
	if !s.needsUpdate(t.Context(), existing, &labelTestData{ID: "002"}, nil) {
		t.Error("needsUpdate() = false, want true when embedded data differs")
	}
}
