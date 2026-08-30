/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"errors"
	"strings"

	"google.golang.org/api/googleapi"
	"google.golang.org/genai"
)

// responseCodeFromError maps a Vertex API error to an HTTP-style response code.
// Returns 0 for nil err (the success path; the caller chooses how to render
// that), the structured HTTP code if the error wraps a genai.APIError or
// *googleapi.Error,
// otherwise the code recovered from gRPC keywords in the error text, or -1
// for errors that don't carry a recognisable status — so the counter never
// silently drops a request.
//
// This is the single source of truth for "what HTTP code did we observe":
// isRetryableVertexError consults this and responseCodeFromMessage so the
// keyword list lives in one place.
func responseCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	// The GenAI SDK returns APIError as a value today, while callers may wrap
	// a pointer themselves. Recognize both shapes before falling back to the
	// legacy googleapi error used by older clients.
	if genaiErrPtr, ok := errors.AsType[*genai.APIError](err); ok {
		return genaiErrPtr.Code
	}
	if genaiErr, ok := errors.AsType[genai.APIError](err); ok {
		return genaiErr.Code
	}
	if apiErr, ok := errors.AsType[*googleapi.Error](err); ok {
		return apiErr.Code
	}
	return responseCodeFromMessage(err.Error())
}

// responseCodeFromMessage recovers an HTTP-style code from gRPC keywords in
// the message text alone (RESOURCE_EXHAUSTED → 429, UNAVAILABLE → 503, …).
// Returns -1 if no keyword matches. Vertex returns these gRPC keywords both
// in plain string errors and inside *googleapi.Error.Message, so this lives
// as its own helper: responseCodeFromError uses it as a fallback when the
// error has no structured code, and isRetryableVertexError consults it
// whenever the structured code (if any) doesn't decide the question — a
// 529 with "Overloaded" in the message is the canonical case.
func responseCodeFromMessage(s string) int {
	switch {
	case strings.Contains(s, "RESOURCE_EXHAUSTED"),
		strings.Contains(s, "ResourceExhausted"),
		strings.Contains(s, "Resource exhausted"),
		strings.Contains(s, "rate limit"),
		strings.Contains(s, "quota exceeded"):
		return 429
	case strings.Contains(s, "Overloaded"):
		return 529
	case strings.Contains(s, "UNAVAILABLE"),
		strings.Contains(s, "unavailable"):
		return 503
	case strings.Contains(s, "DEADLINE_EXCEEDED"),
		strings.Contains(s, "DeadlineExceeded"):
		return 504
	case strings.Contains(s, "CANCELLED"):
		return 499
	case strings.Contains(s, "Internal error"),
		strings.Contains(s, "server error"):
		return 500
	}
	return -1
}

// isRetryableVertexError reports whether err is a transient Vertex AI failure
// worth retrying. An error is retryable if EITHER its structured
// genai.APIError.Code or *googleapi.Error.Code is in the retryable set OR its
// message text matches a retryable gRPC keyword. Both signals are needed:
// Vertex sometimes returns a structured error whose code we don't classify
// as retryable (e.g. 529) but whose message is unambiguously transient (e.g.
// "Overloaded"), and conversely gRPC errors that surface as plain strings
// have no structured code at all.
func isRetryableVertexError(err error) bool {
	if err == nil {
		return false
	}
	switch responseCodeFromError(err) {
	case 429, // rate limited
		499, // client closed request / cancelled (Vertex AI DSQ)
		500, // internal
		502, // bad gateway
		503, // unavailable
		504, // gateway timeout / deadline exceeded
		529: // overloaded
		return true
	}
	// No early return — a structured non-retryable code may still carry an
	// unambiguously transient message.
	// Message-string fallback. Lists every code responseCodeFromMessage
	// can produce that we classify as retryable (note 529, which only
	// arrives via "Overloaded" string matches).
	switch responseCodeFromMessage(err.Error()) {
	case 429, 499, 500, 503, 504, 529:
		return true
	}
	return false
}
