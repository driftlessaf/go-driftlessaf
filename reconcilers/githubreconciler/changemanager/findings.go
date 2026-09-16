/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// maxQuotedBodyBytes bounds a single quoted comment or review body before
	// it is wrapped. A body over the cap is cut on a rune boundary and marked,
	// so a large comment cannot crowd out the rest of the request.
	maxQuotedBodyBytes = 4096
	// truncationMarker is appended to a body cut at maxQuotedBodyBytes.
	truncationMarker = "[truncated]"
	// maxInlinePathBytes bounds a review-thread path placed on a finding's
	// header line. A path to a file that exists in the checkout fits within
	// the Linux PATH_MAX, so the cap never alters one.
	maxInlinePathBytes = 4096
)

var (
	// untrustedMarkerRE matches the boundary tag in either open or close form,
	// case-insensitively, so a quoted body cannot reproduce the wrapper it is
	// placed inside.
	untrustedMarkerRE = regexp.MustCompile(`(?i)</?untrusted-content`)
	// controlRunRE matches a run of characters that render as nothing or as a
	// line break: Unicode control (Cc, the C0 and C1 sets, NEL included),
	// format (Cf: bidirectional controls, zero-width characters, the byte
	// order mark), and the line and paragraph separators (Zl, Zp). A file
	// path has no legitimate use for any of them.
	controlRunRE = regexp.MustCompile(`[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]+`)
)

// sanitizeInlinePath returns a review-thread path fit for a finding's header
// line and name, which sit outside the untrusted-content wrapper around the
// comment bodies. The path names a file in the pull request's tree, so a
// contributor controls it byte for byte: control-character runs collapse to
// one space so the path cannot start a new line of the finding, any boundary
// marker is neutralized so it cannot forge a wrapper, and the result is
// bounded.
func sanitizeInlinePath(p string) string {
	return truncateOnRune(neutralizeUntrustedMarkers(controlRunRE.ReplaceAllString(p, " ")), maxInlinePathBytes)
}

// wrapQuoted bounds a quoted body and encloses it in a boundary element tagged
// with its source, giving a downstream model a fixed anchor for "this span is
// data, not instructions." The body is truncated on a rune boundary, then any
// occurrence of the boundary tag inside it is rewritten so the body cannot forge
// the enclosing markers once the finding is serialized to XML (which escapes the
// body's angle brackets and would otherwise render a forged marker identical to
// a real one).
func wrapQuoted(source, body string) string {
	safe := neutralizeUntrustedMarkers(truncateOnRune(body, maxQuotedBodyBytes))
	return fmt.Sprintf("<untrusted-content source=%q>\n%s\n</untrusted-content>", source, safe)
}

// neutralizeUntrustedMarkers rewrites every occurrence of the boundary tag so a
// quoted body cannot close or reopen the wrapper. The rewrite is loud rather
// than lossy: an attempted boundary stays legible as evidence, and the rest of
// the body passes through unchanged.
func neutralizeUntrustedMarkers(body string) string {
	return untrustedMarkerRE.ReplaceAllString(body, "<neutralized-untrusted-content")
}

// truncateOnRune returns s unchanged when it fits within maxBytes; otherwise it
// cuts s at the last rune boundary at or before maxBytes and appends the
// truncation marker, so the result is always valid UTF-8.
func truncateOnRune(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncationMarker
}

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
// The path is sanitized here because it is written outside the wrapper that
// encloses each comment body.
func formatThreadDetails(path string, line int, isOutdated bool, comments []gqlThreadComment) string {
	var sb strings.Builder

	first := comments[0]

	fmt.Fprintf(&sb, "Review thread by @%s (%s)\n", displayReviewAuthor(first.Author.Login, first.Author.Typename), first.AuthorAssociation)
	fmt.Fprintf(&sb, "Path: %s:%d\n", sanitizeInlinePath(path), line)

	commitAnnotation := first.Commit.Oid
	if isOutdated {
		commitAnnotation += " (outdated)"
	}
	fmt.Fprintf(&sb, "Commit: %s\n", commitAnnotation)

	for _, c := range comments {
		fmt.Fprintf(&sb, "\n[Comment by @%s]\n%s\n", displayReviewAuthor(c.Author.Login, c.Author.Typename), wrapQuoted("review-thread", c.Body))
	}

	return sb.String()
}

// formatReviewBodyDetails builds a human-readable details string for a review body.
func formatReviewBodyDetails(review gqlReviewBodyNode) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Review by @%s (%s) - %s\n", displayReviewAuthor(review.Author.Login, review.Author.Typename), review.AuthorAssociation, review.State)
	fmt.Fprintf(&sb, "Submitted: %s\n", review.SubmittedAt)
	fmt.Fprintf(&sb, "Commit: %s\n", review.Commit.Oid)
	fmt.Fprintf(&sb, "\n%s\n", wrapQuoted("review-body", review.Body))

	return sb.String()
}
