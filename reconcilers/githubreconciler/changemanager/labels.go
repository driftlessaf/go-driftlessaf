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

// normalizeLabel shortens labels that exceed GitHub's limit while retaining a
// stable suffix that distinguishes labels with the same prefix.
func normalizeLabel(label string) string {
	if utf8.RuneCountInString(label) <= maxGitHubLabelLength {
		return label
	}

	digest := sha256.Sum256([]byte(label))
	return string([]rune(label)[:labelPrefixLength]) + hex.EncodeToString(digest[:])[:labelHashLength]
}

func normalizeLabels(labels []string) []string {
	normalized := make([]string, len(labels))
	for i, label := range labels {
		normalized[i] = normalizeLabel(label)
	}
	return normalized
}
