/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
)

func TestRenderReasoningLog(t *testing.T) {
	tests := []struct {
		name    string
		entries []changemanager.ReasoningEntry
		want    string
	}{{
		name: "empty log renders nothing",
	}, {
		name: "single entry",
		entries: []changemanager.ReasoningEntry{{
			CommitHeadline: "fix(libraries/artifacts): add ValidatePageFields to List adapters",
			Summary:        "- validated the page fields on both adapters",
		}},
		want: "**fix(libraries/artifacts): add ValidatePageFields to List adapters**\n" +
			"- validated the page fields on both adapters",
	}, {
		name: "one block per commit, blank-line separated",
		entries: []changemanager.ReasoningEntry{{
			CommitHeadline: "fix(libraries/artifacts): add ValidatePageFields to List adapters",
			Summary:        "- validated the page fields on both adapters",
		}, {
			CommitHeadline: "ci(e2e): retry Phase 2 terraform apply with token refresh",
			Summary:        "- refreshed the token before retrying\n- bounded the retry loop",
		}},
		want: "**fix(libraries/artifacts): add ValidatePageFields to List adapters**\n" +
			"- validated the page fields on both adapters\n" +
			"\n" +
			"**ci(e2e): retry Phase 2 terraform apply with token refresh**\n" +
			"- refreshed the token before retrying\n" +
			"- bounded the retry loop",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderReasoningLog(tt.entries); got != tt.want {
				t.Errorf("renderReasoningLog(): got = %q, want = %q", got, tt.want)
			}
		})
	}
}

func TestCommitHeadline(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want string
	}{{
		name: "single line",
		msg:  "fix: correct frobnication",
		want: "fix: correct frobnication",
	}, {
		name: "multi-line keeps only the first line",
		msg:  "fix: correct frobnication\n\nLonger explanation of the change.",
		want: "fix: correct frobnication",
	}, {
		name: "trailing whitespace trimmed",
		msg:  "fix: correct frobnication \nbody",
		want: "fix: correct frobnication",
	}, {
		name: "empty message",
		msg:  "",
		want: "",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commitHeadline(tt.msg); got != tt.want {
				t.Errorf("commitHeadline(%q): got = %q, want = %q", tt.msg, got, tt.want)
			}
		})
	}
}
