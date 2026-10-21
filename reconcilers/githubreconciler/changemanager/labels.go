/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf8"
)

const (
	maxGitHubLabelLength = 50
	labelPrefixLength    = 40
	labelHashLength      = 10
)

// NormalizeLabel shortens labels that exceed GitHub's limit while retaining a
// stable suffix that distinguishes labels with the same prefix.
//
// Callers that search for a label the change manager wrote must pass the label
// through this function first. GitHub returns an empty result set rather than
// an error for a label that does not exist, so searching for the unshortened
// form of a long label silently matches nothing.
func NormalizeLabel(label string) string {
	if utf8.RuneCountInString(label) <= maxGitHubLabelLength {
		return label
	}

	digest := sha256.Sum256([]byte(label))
	return string([]rune(label)[:labelPrefixLength]) + hex.EncodeToString(digest[:])[:labelHashLength]
}

func normalizeLabels(labels []string) []string {
	normalized := make([]string, len(labels))
	for i, label := range labels {
		normalized[i] = NormalizeLabel(label)
	}
	return normalized
}
