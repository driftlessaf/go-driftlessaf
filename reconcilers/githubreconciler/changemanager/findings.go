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
	sb.WriteString(fmt.Sprintf("Check Run: %s\n", name))
	sb.WriteString(fmt.Sprintf("Status: %s\n", status))
	sb.WriteString(fmt.Sprintf("Conclusion: %s\n", conclusion))
	if title != "" {
		sb.WriteString(fmt.Sprintf("Title: %s\n", title))
	}
	if summary != "" {
		sb.WriteString(fmt.Sprintf("Summary: %s\n", summary))
	}
	if text != "" {
		sb.WriteString(fmt.Sprintf("Details:\n%s\n", text))
	}
	if detailsURL != "" {
		sb.WriteString(fmt.Sprintf("Details URL: %s\n", detailsURL))
	}
	return sb.String()
}

// formatReviewDetails builds a human-readable details string for a code review.
func formatReviewDetails(review gqlReviewNode, activeComments []gqlReviewComment) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Review by @%s (%s) - %s\n", review.Author.Login, review.AuthorAssociation, review.State))
	sb.WriteString(fmt.Sprintf("Submitted: %s\n", review.SubmittedAt))

	if review.Body != "" {
		sb.WriteString(fmt.Sprintf("\nSummary:\n%s\n", review.Body))
	}

	if len(activeComments) > 0 {
		sb.WriteString("\nInline Comments:\n")
		for _, c := range activeComments {
			sb.WriteString(fmt.Sprintf("\n[%s:%d]\n%s\n", c.Path, c.Line, c.Body))
		}
	}

	return sb.String()
}
