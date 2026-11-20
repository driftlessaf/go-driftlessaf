/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"fmt"
	"strings"
)

// reviewBodyReader keeps the full text of review bodies longer than the
// rendered preview at maxQuotedBodyBytes, so every byte of the review can reach
// the agent through the finding logs callback, which callers read by offset.
// A body that fits is not kept: its finding details already hold all of it.
// The reader is read-only after construction, so reads share no cursor.
type reviewBodyReader struct {
	bodies  map[string]string
	threads map[string][]gqlThreadComment
	// conversations holds the full rendering of conversation comments longer
	// than the preview, keyed by finding identifier.
	conversations map[string]string
}

// newReviewBodyReader keeps the bodies of reviews longer than the rendered
// preview, keyed by finding identifier.
func newReviewBodyReader(reviews gqlReviewBodiesConnection) *reviewBodyReader {
	r := &reviewBodyReader{bodies: map[string]string{}, threads: map[string][]gqlThreadComment{}}
	for _, review := range reviews.Nodes {
		if len(review.Body) > maxQuotedBodyBytes {
			r.bodies[reviewBodyIdentifierPrefix+fmt.Sprintf("%d", review.DatabaseId)] = review.Body
		}
	}
	return r
}

// full returns the whole review body for identifier, quoted as untrusted
// content, and whether the identifier names a truncated body at all.
func (r *reviewBodyReader) full(identifier string) (string, bool) {
	if r == nil {
		return "", false
	}
	if text, ok := r.conversations[identifier]; ok {
		return text, true
	}
	if comments, ok := r.threads[identifier]; ok {
		var out strings.Builder
		for _, c := range comments {
			fmt.Fprintf(&out, "[Comment by @%s]\n<untrusted-content source=%q>\n%s\n</untrusted-content>\n", displayReviewAuthor(c.Author.Login, c.Author.Typename), "review-thread", neutralizeUntrustedMarkers(c.Body))
		}
		return out.String(), true
	}
	body, ok := r.bodies[identifier]
	if !ok {
		return "", false
	}
	return fmt.Sprintf("Full review body (%d bytes); the finding details hold only the first %d bytes.\n\n<untrusted-content source=%q>\n%s\n</untrusted-content>\n",
		len(body), maxQuotedBodyBytes, "review-body", neutralizeUntrustedMarkers(body)), true
}
