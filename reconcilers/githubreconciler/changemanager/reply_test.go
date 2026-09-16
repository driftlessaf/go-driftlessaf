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
)

// TestReplyToReviewThread drives the reply mutation through a GraphQL double,
// checking the thread node id and body reach the addPullRequestReviewThreadReply
// input, and that an empty thread id or body is refused before any request.
func TestReplyToReviewThread(t *testing.T) {
	tests := []struct {
		name     string
		threadID string
		body     string
		wantErr  string
		wantSent bool
	}{
		{name: "posts reply for a thread", threadID: "PRRT_123", body: "Fixed by bounding the read.", wantSent: true},
		{name: "empty thread id refused", threadID: "", body: "x", wantErr: "empty review thread id"},
		{name: "empty body refused", threadID: "PRRT_123", body: "", wantErr: "empty reply body"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody string
			gql := newTestGraphQLClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":"C_1"}}}}`)
			}))

			err := replyToReviewThread(t.Context(), gql, tc.threadID, tc.body)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error: got = %v, want containing %q", err, tc.wantErr)
				}
				if gotBody != "" {
					t.Errorf("request sent for a refused reply: %s", gotBody)
				}
				return
			}
			if err != nil {
				t.Fatalf("replyToReviewThread: %v", err)
			}
			if !tc.wantSent {
				return
			}
			if !strings.Contains(gotBody, tc.threadID) {
				t.Errorf("request missing thread id %q: %s", tc.threadID, gotBody)
			}
			if !strings.Contains(gotBody, "bounding the read") {
				t.Errorf("request missing reply body: %s", gotBody)
			}
		})
	}
}
