/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"encoding/json"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

func botReviewFinding(id, login, submitted, commit, body string) callbacks.Finding {
	return callbacks.Finding{
		Kind:       callbacks.FindingKindReview,
		Identifier: id,
		Details: "Review by @" + login + " (NONE) - COMMENTED\nSubmitted: " + submitted + "\nCommit: " + commit + "\n\n" +
			wrapQuoted("review-body", body) + "\n",
	}
}

func TestReviewBodyFingerprint(t *testing.T) {
	a := botReviewFinding("review-body:1", "reviewbot[bot]", "t1", "c1", "Please update the README.")
	b := botReviewFinding("review-body:2", "reviewbot[bot]", "t2", "c2", "Please update the README.")
	c := botReviewFinding("review-body:3", "reviewbot[bot]", "t3", "c3", "Please update the README and fix the race.")
	h := botReviewFinding("review-body:4", "alice", "t4", "c4", "Please update the README.")

	fa, automated, ok := ReviewBodyFingerprint(a)
	if !ok || !automated {
		t.Fatalf("ReviewBodyFingerprint(a) = automated %v, ok %v, want true, true", automated, ok)
	}
	if fb, _, _ := ReviewBodyFingerprint(b); fb != fa {
		t.Errorf("repeat fingerprint = %q, want %q", fb, fa)
	}
	if fc, _, _ := ReviewBodyFingerprint(c); fc == fa {
		t.Error("a different finding must not share a fingerprint")
	}
	for _, body := range []string{"Please  update the README.", "Please update the\nREADME."} {
		f := botReviewFinding("review-body:5", "reviewbot[bot]", "t5", "c5", body)
		if got, _, ok := ReviewBodyFingerprint(f); !ok || got == fa {
			t.Errorf("fingerprint for %q: got = %q, %t, want a distinct fingerprint", body, got, ok)
		}
	}
	fh, automated, ok := ReviewBodyFingerprint(h)
	if !ok || automated {
		t.Errorf("human review: automated %v, ok %v, want false, true", automated, ok)
	}
	if fh == fa {
		t.Error("a human review must not share a fingerprint with a bot's")
	}
	thread := callbacks.Finding{Kind: callbacks.FindingKindReview, Identifier: "PRRT_x", Details: a.Details}
	if _, _, ok := ReviewBodyFingerprint(thread); ok {
		t.Error("a review thread must not fingerprint as a review body")
	}
}

// TestReviewBodyFingerprintRefusesTruncatedBody renders real reviews through
// collectReviewBodyFindings. Two reviews share a prefix longer than the display
// cap and differ only after it, so a hash of the shown text would collide; a
// truncated body must not fingerprint, while a short one still does.
func TestReviewBodyFingerprintRefusesTruncatedBody(t *testing.T) {
	prefix := "Please update the README. " + strings.Repeat("x", maxQuotedBodyBytes)
	review := func(id int64, body string) gqlReviewBodyNode {
		n := gqlReviewBodyNode{DatabaseId: id, AuthorAssociation: "NONE", State: "COMMENTED", Body: body}
		n.Author.Login = "reviewbot"
		n.Author.Typename = "Bot"
		n.Commit.Oid = "head"
		return n
	}
	reviews := gqlReviewBodiesConnection{Nodes: []gqlReviewBodyNode{
		review(1, prefix),
		review(2, prefix+"\nAlso a new security concern."),
		review(3, "Please update the README."),
	}}
	findings := collectReviewBodyFindings(t.Context(), "head", reviews, map[string]struct{}{"reviewbot[bot]": {}})
	if len(findings) != 3 {
		t.Fatalf("findings = %d, want 3", len(findings))
	}
	for _, f := range findings[:2] {
		if _, _, ok := ReviewBodyFingerprint(f); ok {
			t.Errorf("%s: truncated review body fingerprinted, want refused", f.Identifier)
		}
	}
	if _, automated, ok := ReviewBodyFingerprint(findings[2]); !ok || !automated {
		t.Errorf("short review: automated %v, ok %v, want true, true", automated, ok)
	}
}

func TestDropFindings(t *testing.T) {
	s := &Session[struct{}]{findings: []callbacks.Finding{
		botReviewFinding("review-body:1", "reviewbot[bot]", "t", "c", "x"),
		{Kind: callbacks.FindingKindCICheck, Identifier: "check", Details: "x"},
		{Kind: callbacks.FindingKindReview, Identifier: "PRRT_1", Details: "x"},
	}}
	if n := s.DropFindings(func(callbacks.Finding) bool { return true }); n != 1 {
		t.Fatalf("DropFindings() = %d, want 1", n)
	}
	if got := len(s.Findings()); got != 2 {
		t.Errorf("findings left = %d, want 2", got)
	}
}

func TestSetOutOfScopeRecords(t *testing.T) {
	s := &Session[struct{}]{}
	ev := []EvidenceDigest{{Path: "a.go", Digest: "d"}}
	var in []OutOfScopeRecord
	for range MaxOutOfScopeRecords + 3 {
		in = append(in, OutOfScopeRecord{Fingerprint: "f", Revision: "r", Constraint: "c", Evidence: ev})
	}
	in = append(in, OutOfScopeRecord{Fingerprint: "", Revision: "r", Evidence: ev}, OutOfScopeRecord{Fingerprint: "f", Revision: "r"})
	s.SetOutOfScopeRecords(in)
	if got := len(s.OutOfScopeRecords()); got != MaxOutOfScopeRecords {
		t.Errorf("records = %d, want %d", got, MaxOutOfScopeRecords)
	}
}

func TestMetadataOutOfScopeOldBody(t *testing.T) {
	var m metadata
	if err := json.Unmarshal([]byte(`{"commit_budget_baseline":2}`), &m); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}
	if m.OutOfScope != nil {
		t.Errorf("OutOfScope = %v, want nil", m.OutOfScope)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	if want := `{"commit_budget_baseline":2}`; string(b) != want {
		t.Errorf("Marshal() = %s, want %s", b, want)
	}
}

func TestNormalizeEvidencePath(t *testing.T) {
	for in, want := range map[string]string{"a/b.go": "a/b.go", "./a//b.go": "a/b.go", "": "", "/etc/passwd": "", "../x": "", "a/../../x": ""} {
		got, ok := NormalizeEvidencePath(in)
		if got != want || ok != (want != "") {
			t.Errorf("NormalizeEvidencePath(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
}
