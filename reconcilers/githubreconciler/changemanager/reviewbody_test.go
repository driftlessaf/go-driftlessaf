/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

// TestTruncatedReviewBodyTailReadableThroughLogs proves a concern placed after
// the rendered preview's cutoff reaches the agent through the logs callback,
// that repeated details reads return the same preview, and that a short review
// keeps returning its finding details.
func TestTruncatedReviewBodyTailReadableThroughLogs(t *testing.T) {
	const concern = "SAFETY-CONCERN-AFTER-CUTOFF"
	body := "Please update the README. " + strings.Repeat("x", 2*maxQuotedBodyBytes) + concern
	review := func(id int64, body string) gqlReviewBodyNode {
		n := gqlReviewBodyNode{DatabaseId: id, AuthorAssociation: "NONE", State: "COMMENTED", Body: body}
		n.Author.Login = "reviewbot"
		n.Author.Typename = "Bot"
		n.Commit.Oid = "head"
		return n
	}
	reviews := gqlReviewBodiesConnection{Nodes: []gqlReviewBodyNode{review(1, body), review(2, "short")}}
	findings := collectReviewBodyFindings(t.Context(), "head", reviews, map[string]struct{}{"reviewbot[bot]": {}})
	s := &Session[struct{}]{findings: findings, reviewBodies: newReviewBodyReader(reviews), manager: &CM[struct{}]{fullReviewBodyLogs: true}}
	cbs := s.FindingCallbacks()

	if strings.Contains(findings[0].Details, concern) {
		t.Fatal("rendered preview holds the tail, want it cut")
	}
	for range 3 {
		out, err := cbs.GetDetails(t.Context(), callbacks.FindingKindReview, "review-body:1")
		if err != nil {
			t.Fatalf("GetDetails: %v", err)
		}
		if out != findings[0].Details {
			t.Fatal("repeated GetDetails changed the preview")
		}
	}

	logs, err := cbs.GetLogs(t.Context(), callbacks.FindingKindReview, "review-body:1")
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if !strings.Contains(logs, concern) {
		t.Error("concern after the cutoff never reached the agent")
	}
	if !strings.Contains(logs, `<untrusted-content source="review-body">`) {
		t.Errorf("logs are not quoted as untrusted content")
	}

	out, err := cbs.GetDetails(t.Context(), callbacks.FindingKindReview, "review-body:2")
	if err != nil || out != findings[1].Details {
		t.Errorf("short review details = %q, %v, want the finding details", out, err)
	}
	out, err = cbs.GetLogs(t.Context(), callbacks.FindingKindReview, "review-body:2")
	if err != nil || out != findings[1].Details {
		t.Errorf("short review logs = %q, %v, want the finding details", out, err)
	}
}

func TestTruncatedReviewBodyLogsDefaultToPreview(t *testing.T) {
	const concern = "SECRET-CONCERN-AFTER-CUTOFF"
	body := "Please update the README. " + strings.Repeat("x", 2*maxQuotedBodyBytes) + concern
	review := gqlReviewBodyNode{DatabaseId: 1, AuthorAssociation: "NONE", State: "COMMENTED", Body: body}
	review.Author.Login = "reviewbot"
	review.Author.Typename = "Bot"
	review.Commit.Oid = "head"
	reviews := gqlReviewBodiesConnection{Nodes: []gqlReviewBodyNode{review}}
	findings := collectReviewBodyFindings(t.Context(), "head", reviews, map[string]struct{}{"reviewbot[bot]": {}})
	s := &Session[struct{}]{findings: findings, reviewBodies: newReviewBodyReader(reviews), manager: &CM[struct{}]{}}

	logs, err := s.FindingCallbacks().GetLogs(t.Context(), callbacks.FindingKindReview, "review-body:1")
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if logs != findings[0].Details || strings.Contains(logs, concern) {
		t.Errorf("default review logs: tail visible = %t, equal preview = %t", strings.Contains(logs, concern), logs == findings[0].Details)
	}
}

func TestTruncatedThreadLogsContainOnlyTrustedComments(t *testing.T) {
	const suffix = "concern beyond the preview"
	comments := thread("thread-1", false,
		threadComment("stranger", "User", "NONE", "untrusted outsider", ""),
		threadComment("maintainer", "User", "MEMBER", strings.Repeat("x", maxQuotedBodyBytes)+suffix, ""),
	)
	reader := newReviewBodyReader(gqlReviewBodiesConnection{})
	findings, _ := collectThreadFindings(t.Context(), gqlReviewThreadsConnection{Nodes: []gqlReviewThread{comments}}, nil, "", reader)
	s := &Session[struct{}]{findings: findings, reviewBodies: reader, manager: &CM[struct{}]{fullReviewBodyLogs: true}}
	if len(findings) != 1 {
		t.Fatalf("findings: got = %d, want = 1", len(findings))
	}
	if got := strings.Contains(findings[0].Details, suffix); got {
		t.Fatalf("preview contains tail: got = %t, want = false", got)
	}
	full, err := s.FindingCallbacks().GetLogs(t.Context(), callbacks.FindingKindReview, "thread-1")
	if err != nil || !strings.Contains(full, suffix) || strings.Contains(full, "untrusted outsider") {
		t.Errorf("thread logs (tail, outsider, error): got = (%t, %t, %v), want = (true, false, nil)", strings.Contains(full, suffix), strings.Contains(full, "untrusted outsider"), err)
	}
}

func TestTruncatedThreadLogsDefaultToPreview(t *testing.T) {
	const suffix = "CONCERN-AFTER-CUTOFF"
	comments := thread("thread-1", false,
		threadComment("maintainer", "User", "MEMBER", strings.Repeat("x", maxQuotedBodyBytes)+suffix, ""),
	)
	reader := newReviewBodyReader(gqlReviewBodiesConnection{})
	findings, _ := collectThreadFindings(t.Context(), gqlReviewThreadsConnection{Nodes: []gqlReviewThread{comments}}, nil, "", reader)
	s := &Session[struct{}]{findings: findings, reviewBodies: reader, manager: &CM[struct{}]{}}
	logs, err := s.FindingCallbacks().GetLogs(t.Context(), callbacks.FindingKindReview, "thread-1")
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if logs != findings[0].Details || strings.Contains(logs, suffix) {
		t.Errorf("default thread logs: suffix visible = %t, equal preview = %t", strings.Contains(logs, suffix), logs == findings[0].Details)
	}
}
