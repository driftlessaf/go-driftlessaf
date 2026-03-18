/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"fmt"
	"strings"
)

// formatCheckRunDetails builds a human-readable details string for a check run.
func formatCheckRunDetails(name, status, conclusion, title, summary, text, detailsURL string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Check Run: %s\n", name)
	fmt.Fprintf(&sb, "Status: %s\n", status)
	fmt.Fprintf(&sb, "Conclusion: %s\n", conclusion)
	if title != "" {
		fmt.Fprintf(&sb, "Title: %s\n", title)
	}
	if summary != "" {
		fmt.Fprintf(&sb, "Summary: %s\n", summary)
	}
	if text != "" {
		fmt.Fprintf(&sb, "Details:\n%s\n", text)
	}
	if detailsURL != "" {
		fmt.Fprintf(&sb, "Details URL: %s\n", detailsURL)
	}
	return sb.String()
}

// formatThreadDetails builds a human-readable details string for a review thread.
// Includes commit SHA and outdated status so the agent can contextualize via history tools.
func formatThreadDetails(path string, line int, isOutdated bool, comments []gqlThreadComment) string {
	var sb strings.Builder

	first := comments[0]

	fmt.Fprintf(&sb, "Review thread by @%s (%s)\n", first.Author.Login, first.AuthorAssociation)
	fmt.Fprintf(&sb, "Path: %s:%d\n", path, line)

	commitAnnotation := first.Commit.Oid
	if isOutdated {
		commitAnnotation += " (outdated)"
	}
	fmt.Fprintf(&sb, "Commit: %s\n", commitAnnotation)

	for _, c := range comments {
		fmt.Fprintf(&sb, "\n[Comment by @%s]\n%s\n", c.Author.Login, c.Body)
	}

	return sb.String()
}

// formatReviewBodyDetails builds a human-readable details string for a review body.
func formatReviewBodyDetails(review gqlReviewBodyNode) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Review by @%s (%s) - %s\n", review.Author.Login, review.AuthorAssociation, review.State)
	fmt.Fprintf(&sb, "Submitted: %s\n", review.SubmittedAt)
	fmt.Fprintf(&sb, "Commit: %s\n", review.Commit.Oid)
	fmt.Fprintf(&sb, "\n%s\n", review.Body)

	return sb.String()
}
