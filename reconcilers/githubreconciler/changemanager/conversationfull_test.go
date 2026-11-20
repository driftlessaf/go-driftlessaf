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

// TestConversationCommentFullTextKnown proves a long conversation comment has
// full text in FindingsFullText and the logs callback, while a short one
// ending in the truncation marker is left as is.
func TestConversationCommentFullTextKnown(t *testing.T) {
	const tail = "TAIL-AFTER-CUTOFF"
	long := gqlConversationComment{DatabaseId: 1, AuthorAssociation: "MEMBER", Body: strings.Repeat("x", 2*maxQuotedBodyBytes) + tail}
	short := gqlConversationComment{DatabaseId: 2, AuthorAssociation: "MEMBER", Body: "clipped output" + truncationMarker}
	reader := newReviewBodyReader(gqlReviewBodiesConnection{})
	reader.conversations = map[string]string{conversationIdentifier(1): formatConversation(long, wrapQuotedFull)}
	s := &Session[struct{}]{
		findings: []callbacks.Finding{
			{Kind: callbacks.FindingKindReview, Identifier: conversationIdentifier(1), Details: formatConversationDetails(long)},
			{Kind: callbacks.FindingKindReview, Identifier: conversationIdentifier(2), Details: formatConversationDetails(short)},
		},
		reviewBodies: reader,
		manager:      &CM[struct{}]{fullReviewBodyLogs: true},
	}
	got := s.FindingsFullText()
	if strings.Contains(s.findings[0].Details, tail) {
		t.Fatal("preview holds the tail, want it cut")
	}
	if !strings.Contains(got[0].Details, tail) {
		t.Errorf("full text of long comment: got = %q, want text containing %q", got[0].Details, tail)
	}
	if got[1].Details != s.findings[1].Details {
		t.Errorf("short comment changed: got = %q, want = %q", got[1].Details, s.findings[1].Details)
	}
	out, err := s.FindingCallbacks().GetLogs(t.Context(), callbacks.FindingKindReview, conversationIdentifier(1))
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if !strings.Contains(out, tail) {
		t.Errorf("GetLogs: got = %q, want text containing %q", out, tail)
	}
}
