/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

// TestFindingCallbacksResolveReplyValidateIdentifier checks that the Resolve
// and Reply callbacks act only on an identifier this session discovered. A
// known review thread identifier is queued and reaches the GraphQL mutation
// when the session flushes after a push; an identifier the session never
// surfaced, or a review-body identifier, is refused before anything is queued,
// so a caller cannot steer a mutation at an arbitrary review thread.
func TestFindingCallbacksResolveReplyValidateIdentifier(t *testing.T) {
	const knownThread = "PRRT_known"
	reviewBodyID := reviewBodyIdentifierPrefix + "5"

	tests := []struct {
		name       string
		op         string // "resolve" or "reply"
		identifier string
		wantErr    string
		wantSent   bool
	}{
		{name: "resolve known thread is queued and sent on flush", op: "resolve", identifier: knownThread, wantSent: true},
		{name: "resolve unknown thread rejected", op: "resolve", identifier: "PRRT_unknown", wantErr: "finding not found"},
		{name: "resolve review body rejected", op: "resolve", identifier: reviewBodyID, wantErr: "cannot resolve review body findings"},
		{name: "reply known thread is queued and sent on flush", op: "reply", identifier: knownThread, wantSent: true},
		{name: "reply unknown thread rejected", op: "reply", identifier: "PRRT_unknown", wantErr: "finding not found"},
		{name: "reply review body rejected", op: "reply", identifier: reviewBodyID, wantErr: "cannot reply to review body findings"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sent bool
			gql := newTestGraphQLClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sent = true
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"data":{}}`)
			}))

			s := &Session[testData]{
				manager:   &CM[testData]{findingReplies: true},
				gqlClient: gql,
				findings: []callbacks.Finding{
					{Kind: callbacks.FindingKindReview, Identifier: knownThread},
					{Kind: callbacks.FindingKindReview, Identifier: reviewBodyID},
				},
			}
			cb := s.FindingCallbacks()

			var err error
			switch tc.op {
			case "resolve":
				err = cb.Resolve(t.Context(), tc.identifier)
			case "reply":
				err = cb.Reply(t.Context(), tc.identifier, "a short disposition")
			default:
				t.Fatalf("unknown op %q", tc.op)
			}

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error: got = %v, want containing %q", err, tc.wantErr)
				}
				if sent {
					t.Errorf("GraphQL request sent for a refused %s of %q", tc.op, tc.identifier)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", tc.op, err)
			}
			if sent {
				t.Errorf("GraphQL request sent during %s of %q; thread actions must wait for the push", tc.op, tc.identifier)
			}
			s.flushThreadActions(t.Context(), true)
			if tc.wantSent && !sent {
				t.Errorf("no GraphQL request sent for %s of %q after the flush", tc.op, tc.identifier)
			}
		})
	}
}
