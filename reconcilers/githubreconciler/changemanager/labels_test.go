/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
)

func TestNormalizeLabel(t *testing.T) {
	tests := []struct {
		name  string
		label string
		want  string
	}{
		{
			name:  "short label unchanged",
			label: "linux-aws",
			want:  "linux-aws",
		},
		{
			name:  "fifty character label unchanged",
			label: strings.Repeat("a", maxGitHubLabelLength),
			want:  strings.Repeat("a", maxGitHubLabelLength),
		},
		{
			name:  "long label gets prefix and hash",
			label: "damienaicheh-update-android-manifest-package-action",
			want:  "damienaicheh-update-android-manifest-pac62d884ef48",
		},
		{
			name:  "another long label gets a distinct hash",
			label: "aws-actions-vulnerability-scan-github-action-for-amazon-inspector",
			want:  "aws-actions-vulnerability-scan-github-ac0bfa73b82e",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeLabel(tt.label)
			if got != tt.want {
				t.Errorf("normalizeLabel(%q): got = %q, want = %q", tt.label, got, tt.want)
			}
			if utf8.RuneCountInString(got) > maxGitHubLabelLength {
				t.Errorf("normalized label length: got = %d, want <= %d", utf8.RuneCountInString(got), maxGitHubLabelLength)
			}
		})
	}
}

func TestNormalizeLabelsDoesNotMutateInput(t *testing.T) {
	input := []string{"automated-pr", "damienaicheh-update-android-manifest-package-action"}
	wantInput := append([]string(nil), input...)
	want := []string{"automated-pr", "damienaicheh-update-android-manifest-pac62d884ef48"}

	got := normalizeLabels(input)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("normalizeLabels (-want, +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantInput, input); diff != "" {
		t.Errorf("input mutated (-want, +got):\n%s", diff)
	}
}
