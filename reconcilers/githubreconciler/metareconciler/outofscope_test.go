/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"github.com/google/go-github/v88/github"
)

func reviewBody(login string) callbacks.Finding {
	return callbacks.Finding{
		Kind:       callbacks.FindingKindReview,
		Identifier: "review-body:1",
		Details:    "Review by @" + login + " (NONE) - COMMENTED\nSubmitted: t\nCommit: c\n\n<untrusted-content source=\"review-body\">\nPlease update the README.\n</untrusted-content>\n",
	}
}

func testIssue(body string) *github.Issue {
	return &github.Issue{Title: new("title"), Body: new(body)}
}

const constraint = "Do not edit the README."

func TestWrappedConstraintKeepsItsException(t *testing.T) {
	const body = "## Constraints\n\n- Do not edit README.md\n  unless this change makes it inaccurate."
	if IsExplicitConstraint(body, "Do not edit README.md") {
		t.Error("accepted a partial constraint without its exception")
	}
	if !IsExplicitConstraint(body, "Do not edit README.md unless this change makes it inaccurate.") {
		t.Error("rejected the complete wrapped constraint")
	}
}

func TestOutOfScopeRecordValidation(t *testing.T) {
	issue := testIssue("Fix the bug.\n\n## Constraints\n\n" + constraint)
	blobs := map[string]string{"a.go": "sha1"}
	blob := func(p string) string { return blobs[p] }
	bot := reviewBody("bot[bot]")
	findings := []callbacks.Finding{bot}
	dec := OutOfScopeDecision{Identifier: "review-body:1", Constraint: constraint, Evidence: []string{"./a.go"}, WholeReview: true}
	partial := dec
	partial.WholeReview = false

	tests := []struct {
		name       string
		decision   OutOfScopeDecision
		findings   []callbacks.Finding
		blob       func(string) string
		wantRecord bool
	}{
		{"grounded bot review", dec, findings, blob, true},
		{"not the whole review", partial, findings, blob, false},
		{"ungrounded constraint", OutOfScopeDecision{Identifier: "review-body:1", Constraint: "do not touch auth", Evidence: []string{"a.go"}, WholeReview: true}, findings, blob, false},
		{"single word substring", OutOfScopeDecision{Identifier: "review-body:1", Constraint: "the", Evidence: []string{"a.go"}, WholeReview: true}, findings, blob, false},
		{"single token from the issue", OutOfScopeDecision{Identifier: "review-body:1", Constraint: "Fix", Evidence: []string{"a.go"}, WholeReview: true}, findings, blob, false},
		{"fragment of a sentence", OutOfScopeDecision{Identifier: "review-body:1", Constraint: "edit the README", Evidence: []string{"a.go"}, WholeReview: true}, findings, blob, false},
		{"case changed", OutOfScopeDecision{Identifier: "review-body:1", Constraint: "do not edit the readme.", Evidence: []string{"a.go"}, WholeReview: true}, findings, blob, false},
		{"requirement line outside constraints", OutOfScopeDecision{Identifier: "review-body:1", Constraint: "Fix the bug", Evidence: []string{"a.go"}, WholeReview: true}, findings, blob, false},
		{"title is not a constraint", OutOfScopeDecision{Identifier: "review-body:1", Constraint: "Update the README", Evidence: []string{"a.go"}, WholeReview: true}, findings, blob, false},
		{"human review", dec, []callbacks.Finding{reviewBody("alice")}, blob, false},
		{"no evidence", OutOfScopeDecision{Identifier: "review-body:1", Constraint: constraint, WholeReview: true}, findings, blob, false},
		{"unreadable evidence", dec, findings, func(string) string { return "" }, false},
		{"unknown identifier", OutOfScopeDecision{Identifier: "review-body:9", Constraint: constraint, Evidence: []string{"a.go"}, WholeReview: true}, findings, blob, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildOutOfScopeRecords(nil, []OutOfScopeDecision{tc.decision}, tc.findings, issue, intentRevision(issue, ""), tc.blob)
			if (len(got) == 1) != tc.wantRecord {
				t.Fatalf("records = %v, want record %v", got, tc.wantRecord)
			}
		})
	}
}

func TestOutOfScopeDuplicateDecisionsBoundBlobLookups(t *testing.T) {
	issue := testIssue("Fix the bug.\n\n## Constraints\n\n" + constraint)
	findings := []callbacks.Finding{reviewBody("bot[bot]")}
	dec := OutOfScopeDecision{Identifier: "review-body:1", Constraint: constraint, Evidence: []string{"a.go", "b.go"}, WholeReview: true}
	decisions := make([]OutOfScopeDecision, 1000)
	for i := range decisions {
		decisions[i] = dec
	}
	calls := 0
	blob := func(string) string { calls++; return "sha" }
	got := buildOutOfScopeRecords(nil, decisions, findings, issue, intentRevision(issue, ""), blob)
	if len(got) != 1 {
		t.Fatalf("records = %v, want 1", got)
	}
	if calls > 2 {
		t.Errorf("blob calls = %d, want at most 2", calls)
	}
}

func TestOutOfScopePrunesOldRevision(t *testing.T) {
	old := []changemanager.OutOfScopeRecord{{Fingerprint: "f", Revision: "old", Evidence: []changemanager.EvidenceDigest{{Path: "a", Digest: "d"}}}}
	if got := buildOutOfScopeRecords(old, nil, nil, testIssue("x"), intentRevision(testIssue("x"), ""), func(string) string { return "" }); len(got) != 0 {
		t.Errorf("records = %v, want none", got)
	}
}

func TestRejectedConstraintHeadingDoesNotGroundDecision(t *testing.T) {
	const constraint = "Do not edit the README."
	for _, heading := range []string{"## Rejected constraints", "Rejected constraints\n---", "Rejected constraints\n==="} {
		for _, prefix := range []string{"- ", "- Constraint: "} {
			issue := testIssue(heading + "\n" + prefix + constraint)
			if got := IsExplicitConstraint(issue.GetBody(), constraint); got {
				t.Errorf("grounded in %q: got = %t, want false", issue.GetBody(), got)
			}
		}
	}
}

func TestMarkdownExamplesAndHeadingsDoNotGroundDecisions(t *testing.T) {
	for _, body := range []string{
		"## Constraints\nRejected alternatives\n---",
		"    ## Constraints\n    Rejected alternatives",
		"<!--\n## Constraints\nRejected alternatives\n-->",
		"Migration notes <!-- archived example\nConstraint: Rejected alternatives\n-->",
		"> ## Constraints\n> Rejected alternatives",
		"```markdown\n## Constraints\nRejected alternatives\n```",
		"~~~markdown\n## Constraints\nRejected alternatives\n~~~",
	} {
		if IsExplicitConstraint(body, "Rejected alternatives") {
			t.Errorf("accepted heading or example as a constraint in %q", body)
		}
	}
}
