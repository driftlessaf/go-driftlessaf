/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
)

func TestLatestTrustedAttachment(t *testing.T) {
	const title = "planner_state"
	now := time.Now()
	att := func(id, creator string, age time.Duration) linearreconciler.Attachment {
		a := linearreconciler.Attachment{ID: id, Title: title, URL: "https://uploads.linear.app/" + id, CreatedAt: now.Add(-age)}
		if creator != "" {
			a.Creator = &linearreconciler.User{ID: creator}
		}
		return a
	}

	tests := []struct {
		name  string
		nodes []linearreconciler.Attachment
		want  string
	}{
		{
			name:  "forged newer attachment is ignored",
			nodes: []linearreconciler.Attachment{att("bot", "bot-id", time.Hour), att("forged", "attacker-id", 0)},
			want:  "bot",
		},
		{
			name:  "only forged attachment yields nothing",
			nodes: []linearreconciler.Attachment{att("forged", "attacker-id", 0)},
			want:  "",
		},
		{
			name:  "missing creator is untrusted",
			nodes: []linearreconciler.Attachment{att("nocreator", "", 0)},
			want:  "",
		},
		{
			name:  "newest trusted wins",
			nodes: []linearreconciler.Attachment{att("old", "bot-id", time.Hour), att("new", "bot-id", 0)},
			want:  "new",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issue := &linearreconciler.Issue{}
			issue.Attachments.Nodes = tc.nodes
			got := latestTrustedAttachment(t.Context(), issue, title, "bot-id")
			switch {
			case tc.want == "" && got != nil:
				t.Errorf("got %q, want nil", got.ID)
			case tc.want != "" && (got == nil || got.ID != tc.want):
				t.Errorf("got %v, want %q", got, tc.want)
			}
		})
	}
}
