/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	gogit "github.com/go-git/go-git/v5"
)

// IssueData is the desired-state type NewIssues files issues with. Issues
// match across passes by Key alone (see Equal); the remaining fields refresh
// a matched issue's body in place when they change. The templates on the
// issuemanager render its fields.
type IssueData struct {
	// Key uniquely identifies the issue within its path. It must be stable
	// across passes: a changed key reads as "old finding resolved, new
	// finding discovered", closing the old issue (and, downstream, any
	// in-flight fix PR driven by it).
	Key string `json:"key"`

	// Path is the reconciled path the findings belong to.
	Path string `json:"path"`

	// Rule is the shared rule of the grouped diagnostics (GroupByRule);
	// empty for groupings that span rules.
	Rule string `json:"rule,omitempty"`

	// Diagnostics are the individual findings backing this issue, in
	// deterministic order.
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Equal implements issuemanager.Comparable: issues match by Key alone.
func (d IssueData) Equal(o IssueData) bool { return d.Key == o.Key }

// Grouping converts one path's diagnostics into desired issue data. Each
// returned IssueData must have a Key that is unique within the result and
// stable across passes (avoid line numbers, which drift as files change).
// Implementations must be deterministic: the embedded data drives in-place
// issue updates, so a reordered result reads as a changed issue.
type Grouping func(res *githubreconciler.Resource, diags []Diagnostic) []*IssueData

// GroupByRule groups diagnostics into one issue per rule, keyed by the rule
// name — line-free stable keys, one issue per independently fixable concern.
// It is the default grouping for NewIssues.
func GroupByRule(res *githubreconciler.Resource, diags []Diagnostic) []*IssueData {
	byRule := make(map[string][]Diagnostic, len(diags))
	for _, d := range diags {
		byRule[d.Rule] = append(byRule[d.Rule], d)
	}
	out := make([]*IssueData, 0, len(byRule))
	for rule, ds := range byRule {
		out = append(out, &IssueData{
			Key:         rule,
			Path:        res.Path,
			Rule:        rule,
			Diagnostics: sortDiagnostics(ds),
		})
	}
	slices.SortFunc(out, func(a, b *IssueData) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

// GroupByPath aggregates all of a path's diagnostics into a single issue,
// keyed by the path itself.
func GroupByPath(res *githubreconciler.Resource, diags []Diagnostic) []*IssueData {
	if len(diags) == 0 {
		return nil
	}
	return []*IssueData{{
		Key:         res.Path,
		Path:        res.Path,
		Diagnostics: sortDiagnostics(diags),
	}}
}

// sortDiagnostics returns a sorted copy so the embedded issue data is
// deterministic across passes.
func sortDiagnostics(diags []Diagnostic) []Diagnostic {
	out := slices.Clone(diags)
	slices.SortFunc(out, func(a, b Diagnostic) int {
		if c := cmp.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Line, b.Line); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Rule, b.Rule); c != 0 {
			return c
		}
		return cmp.Compare(a.Message, b.Message)
	})
	return out
}

// deriveIssues runs the analyzer over the path — seeded with the prior
// findings from the currently open issues — and converts its diagnostics
// into the desired issue set: diagnostics marked Fixed are dropped (see
// NewIssues), the grouping computes the issue data, and the resulting keys
// are validated to be unique.
func deriveIssues(ctx context.Context, analyzer Analyzer, grouping Grouping, res *githubreconciler.Resource, wt *gogit.Worktree, prior []Diagnostic) ([]*IssueData, []Diagnostic, error) {
	diags, err := analyzer.Analyze(ctx, wt, []string{res.Path}, prior...)
	if err != nil {
		return nil, nil, fmt.Errorf("run analyzer: %w", err)
	}
	diags = slices.DeleteFunc(diags, func(d Diagnostic) bool { return d.Fixed })
	desired := grouping(res, diags)
	seen := make(map[string]struct{}, len(desired))
	for _, d := range desired {
		if _, dup := seen[d.Key]; dup {
			return nil, nil, fmt.Errorf("grouping produced duplicate key %q", d.Key)
		}
		seen[d.Key] = struct{}{}
	}
	return desired, diags, nil
}
