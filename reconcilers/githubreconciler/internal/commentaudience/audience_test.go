/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commentaudience

import (
	"strings"
	"testing"
)

func TestForSomeoneElse(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		botLogin string
		want     bool
	}{{
		name:     "no mention",
		body:     "please use 1.2.3",
		botLogin: "example-app",
		want:     false,
	}, {
		name:     "mentions a person",
		body:     "@example-user can you take a look?",
		botLogin: "example-app",
		want:     true,
	}, {
		name:     "mentions the bot",
		body:     "@example-app please rebase",
		botLogin: "example-app",
		want:     false,
	}, {
		name:     "mentions the bot whose login has a bot suffix",
		body:     "@example-app please rebase",
		botLogin: "example-app[bot]",
		want:     false,
	}, {
		name:     "mentions the bot by its full bot login",
		body:     "@example-app[bot] please rebase",
		botLogin: "example-app",
		want:     false,
	}, {
		name:     "mentions the bot in a different case",
		body:     "@Example-App please rebase",
		botLogin: "example-app",
		want:     false,
	}, {
		name:     "mentions the bot and a person",
		body:     "@example-app please rebase, and @example-user please review",
		botLogin: "example-app",
		want:     true,
	}, {
		name:     "package name in inline code",
		body:     "bump `@types/node` to the next major",
		botLogin: "example-app",
		want:     false,
	}, {
		name:     "mention inside a fenced code block",
		body:     "the log says:\n```\nerror from @example-user\n```\nplease fix",
		botLogin: "example-app",
		want:     false,
	}, {
		name:     "mention after a fenced code block counts",
		body:     "```\ncode\n```\n@example-user thoughts?",
		botLogin: "example-app",
		want:     true,
	}, {
		name:     "mention in a quoted line",
		body:     "> cc @example-user\nplease use 1.2.3",
		botLogin: "example-app",
		want:     false,
	}, {
		name:     "email address",
		body:     "mail a@example.com",
		botLogin: "example-app",
		want:     false,
	}, {
		name:     "mentions a team",
		body:     "@example-org/example-team please look",
		botLogin: "example-app",
		want:     true,
	}, {
		name:     "empty bot login with a mention",
		body:     "@example-app please rebase",
		botLogin: "",
		want:     true,
	}, {
		name:     "empty body",
		body:     "",
		botLogin: "example-app",
		want:     false,
	}, {
		// A known limit: an npm scoped package name outside code looks the same
		// as an org/team mention, so it counts. Putting it in backticks avoids
		// this.
		name:     "unquoted scoped package name counts as a team mention",
		body:     "bump @scope/package to 2.0",
		botLogin: "example-app",
		want:     true,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ForSomeoneElse(tc.body, tc.botLogin); got != tc.want {
				t.Errorf("ForSomeoneElse(%q, %q): got = %v, want = %v", tc.body, tc.botLogin, got, tc.want)
			}
		})
	}
}

func FuzzForSomeoneElse(f *testing.F) {
	f.Add("@example-user can you take a look?", "example-app")
	f.Add("```\n@example-user\n```", "example-app[bot]")
	f.Add("> cc @example-user", "")
	f.Add("mail a@example.com `@types/node`", "example-app")
	f.Fuzz(func(t *testing.T, body, botLogin string) {
		got := ForSomeoneElse(body, botLogin)
		// Without an @ there is nothing to mention.
		if got && !strings.Contains(body, "@") {
			t.Errorf("ForSomeoneElse(%q, %q): got = true for a body with no @", body, botLogin)
		}
	})
}
