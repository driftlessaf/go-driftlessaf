/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commentack

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestPending(t *testing.T) {
	now := time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)
	trusted := func(association string) bool {
		switch association {
		case "OWNER", "MEMBER", "COLLABORATOR":
			return true
		}
		return false
	}
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	tests := []struct {
		name     string
		comments []Comment
		// since is the head commit cutoff; zero means none.
		since time.Time
		want  []string
	}{{
		name:     "created before since",
		comments: []Comment{{NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(2 * time.Hour)}},
		since:    ago(time.Hour),
	}, {
		// GitHub keeps whole seconds, so a comment in the same second as the
		// commit cannot be shown to come after it.
		name:     "created at since",
		comments: []Comment{{NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(time.Hour)}},
		since:    ago(time.Hour),
	}, {
		name:     "created after since",
		comments: []Comment{{NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(time.Minute)}},
		since:    ago(time.Hour),
		want:     []string{"a"},
	}, {
		// The bot acts on an open thread whatever its age, so the cutoff for the
		// conversation does not apply to it.
		name:     "inline comment created before since",
		comments: []Comment{{NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(2 * time.Hour), Inline: true}},
		since:    ago(time.Hour),
		want:     []string{"a"},
	}, {
		name:     "zero since is no cutoff",
		comments: []Comment{{NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(23 * time.Hour)}},
		want:     []string{"a"},
	}, {
		// Inside the 24-hour window is not enough: the bot does not act on a
		// comment left before the head commit, so it must not get a 👀.
		name:     "created before since but inside the window",
		comments: []Comment{{NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(3 * time.Hour)}},
		since:    ago(2 * time.Hour),
	}, {
		name:     "since does not bring back a comment older than the window",
		comments: []Comment{{NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(25 * time.Hour)}},
		since:    ago(48 * time.Hour),
	}, {
		name:     "recent comment from a trusted person",
		comments: []Comment{{NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(time.Hour)}},
		want:     []string{"a"},
	}, {
		name:     "comment from a bot",
		comments: []Comment{{NodeID: "a", AuthorType: "Bot", AuthorAssociation: "NONE", CreatedAt: ago(time.Hour)}},
	}, {
		name:     "author was deleted",
		comments: []Comment{{NodeID: "a", AuthorType: "", AuthorAssociation: "OWNER", CreatedAt: ago(time.Hour)}},
	}, {
		name:     "untrusted association",
		comments: []Comment{{NodeID: "a", AuthorType: "User", AuthorAssociation: "CONTRIBUTOR", CreatedAt: ago(time.Hour)}},
	}, {
		name:     "minimized comment",
		comments: []Comment{{NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(time.Hour), IsMinimized: true}},
	}, {
		name:     "older than the window",
		comments: []Comment{{NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(25 * time.Hour)}},
	}, {
		name: "a bot already reacted with eyes",
		comments: []Comment{{
			NodeID: "a", AuthorType: "User", AuthorAssociation: "MEMBER", CreatedAt: ago(time.Hour),
			EyesFromBot: true,
		}},
	}, {
		name: "a person reacted with eyes",
		comments: []Comment{{
			NodeID: "a", AuthorType: "User", AuthorAssociation: "MEMBER", CreatedAt: ago(time.Hour),
			EyesFromBot: false,
		}},
		want: []string{"a"},
	}, {
		name: "mentions someone other than the bot",
		comments: []Comment{{
			NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(time.Hour),
			Body: "@example-user can you take a look?",
		}},
	}, {
		name: "mentions the bot",
		comments: []Comment{{
			NodeID: "a", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(time.Hour),
			Body: "@example-app please rebase",
		}},
		want: []string{"a"},
	}, {
		name: "oldest first",
		comments: []Comment{
			{NodeID: "new", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: ago(time.Minute)},
			{NodeID: "old", AuthorType: "User", AuthorAssociation: "COLLABORATOR", CreatedAt: ago(2 * time.Hour)},
		},
		want: []string{"old", "new"},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Pending(tc.comments, Options{Now: now, Window: 24 * time.Hour, Since: tc.since, Trusted: trusted, BotLogin: "example-app"})
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Pending (-want, +got):\n%s", diff)
			}
		})
	}
}
