/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"strings"
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

// The PR title becomes the squash commit's subject on the default branch, so
// it names the change that built the branch, not the latest fix-up stacked on
// it, and a rebuild from the default branch names its change afresh.
func TestPRHeadline(t *testing.T) {
	tests := []struct {
		name             string
		recorded         string
		current          string
		stacksOnPRBranch bool
		want             string
	}{{
		name:    "first run uses current headline",
		current: "fix(make-docs): add retry logic for go install",
		want:    "fix(make-docs): add retry logic for go install",
	}, {
		name: "no recorded and no current headline",
		want: "",
	}, {
		name:     "analyzer-only rebuild leaves headline empty for path fallback",
		recorded: "fix(redpanda): bump epoch to 8",
		want:     "",
	}, {
		name:             "recorded headline wins while the branch's change remains",
		recorded:         "feat(gharchive): add example_test.go with Example functions",
		current:          "fix(lint): remove unused import",
		stacksOnPRBranch: true,
		want:             "feat(gharchive): add example_test.go with Example functions",
	}, {
		name:             "analyzer-only fix-up keeps the recorded headline",
		recorded:         "feat(gharchive): add example_test.go with Example functions",
		stacksOnPRBranch: true,
		want:             "feat(gharchive): add example_test.go with Example functions",
	}, {
		// PRs whose body predates the recorded headline have none to keep.
		name:             "fix-up with no recorded headline uses current headline",
		current:          "fix(lint): remove unused import",
		stacksOnPRBranch: true,
		want:             "fix(lint): remove unused import",
	}, {
		name:     "rebuild replaces discarded headline",
		recorded: "fix(redpanda): bump epoch to 8",
		current:  "fix(redpanda): migrate to go/build/v2",
		want:     "fix(redpanda): migrate to go/build/v2",
	}, {
		name:             "overlong headline truncated with ellipsis",
		recorded:         "fix(x): " + strings.Repeat("a", 200),
		stacksOnPRBranch: true,
		want:             "fix(x): " + strings.Repeat("a", prHeadlineMaxChars-9) + "…",
	}, {
		// Multibyte runes straddle the cap so byte-based slicing would cut
		// mid-sequence and produce invalid UTF-8 instead of whole runes.
		name:             "overlong multibyte headline truncated on rune boundary",
		recorded:         "fix(x): " + strings.Repeat("é", 200),
		stacksOnPRBranch: true,
		want:             "fix(x): " + strings.Repeat("é", prHeadlineMaxChars-9) + "…",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prHeadline(tt.recorded, tt.current, tt.stacksOnPRBranch); got != tt.want {
				t.Errorf("prHeadline(): got = %q, want = %q", got, tt.want)
			}
		})
	}
}
