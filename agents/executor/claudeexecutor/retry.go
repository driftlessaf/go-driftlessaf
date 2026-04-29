/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"errors"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// responseCodeFromError maps a Claude API error to an HTTP-style response code.
// Returns 0 for nil err (the success path; the caller chooses how to render
// that), the structured StatusCode if the error wraps an *anthropic.Error,
// otherwise the code recovered from the SSE error_type string, or -1 for
// errors that don't carry a recognisable status — so the counter never
// silently drops a request.
//
// This is the single source of truth for "what HTTP code did we observe":
// isRetryableClaudeError consults this and responseCodeFromMessage so the
// keyword list lives in one place.
func responseCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return responseCodeFromMessage(err.Error())
}

// responseCodeFromMessage recovers an HTTP-style code from Anthropic
// error_type keywords embedded in plain fmt.Errorf SSE-streaming errors:
// the ssestream package emits "received error while streaming: <json>" with
// the type string in the JSON body. Per
// https://docs.anthropic.com/en/api/errors:
//   - rate_limit_error → 429
//   - api_error        → 500
//   - overloaded_error → 529
//
// Returns -1 if no keyword matches. Lives as its own helper so the keyword
// list is defined once and shared by responseCodeFromError (the metric
// path) and isRetryableClaudeError (the retry-decision path).
func responseCodeFromMessage(s string) int {
	switch {
	case strings.Contains(s, "rate_limit_error"):
		return 429
	case strings.Contains(s, "overloaded_error"):
		return 529
	case strings.Contains(s, "api_error"):
		return 500
	}
	return -1
}

// responseCodeAttr formats a code from responseCodeFromError as a string
// attribute for the genai.api.requests counter. Mirrors the response_code
// label on serviceruntime.googleapis.com/api/request_count: "200" for
// success, the numeric code for everything we recognise, "unknown" for
// errors that don't carry a status (so they still get counted).
func responseCodeAttr(code int) string {
	switch {
	case code == 0:
		return "200"
	case code < 0:
		return "unknown"
	default:
		return strconv.Itoa(code)
	}
}

// isRetryableClaudeError reports whether err is a transient Claude API
// failure worth retrying. *anthropic.Error.StatusCode is treated as
// authoritative when present, with no fall-through to keyword matching —
// the SDK populates StatusCode for every typed API failure, and the
// keyword list only exists to handle SSE-streaming errors that surface as
// plain fmt.Errorf with the error_type embedded in JSON.
func isRetryableClaudeError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch responseCodeFromError(err) {
		case 429, 503, 504, 529:
			return true
		}
		return false
	}
	// SSE-string fallback. Lists every code responseCodeFromMessage can
	// produce; api_error / 500 is included here even though structured
	// 500s are not retried, because Anthropic's docs classify api_error
	// as transient.
	switch responseCodeFromMessage(err.Error()) {
	case 429, 500, 529:
		return true
	}
	return false
}
