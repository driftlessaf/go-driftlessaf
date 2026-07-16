/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// responseCodeFromError maps a Claude API error to an HTTP-style response code.
// Returns 0 for nil err (the success path; the caller chooses how to render
// that). If the error wraps an *anthropic.Error the structured StatusCode is
// authoritative, except when it is 200 (or 0): the API delivers mid-stream
// failures as SSE error events on an already-open stream, so the SDK stamps
// them with the HTTP 200 of the successful stream open and the real failure
// lives only in the body error type — inStreamErrorCode recovers the code for
// those. For everything else the code is recovered from the SSE error_type
// string, or -1 for errors that don't carry a recognisable status — so the
// counter never silently drops a request.
//
// This is the single source of truth for "what HTTP code did we observe":
// isRetryableClaudeError consults inStreamErrorCode and
// responseCodeFromMessage so each mapping lives in one place.
func responseCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		if code := inStreamErrorCode(apiErr); code > 0 {
			return code
		}
		return apiErr.StatusCode
	}
	return responseCodeFromMessage(err.Error())
}

// inStreamErrorCode maps an *anthropic.Error delivered as an SSE error event
// on an already-open stream to the HTTP-style code documented for its body
// error type. Such errors carry the HTTP 200 of the successful stream open in
// StatusCode (the SDK copies it from the response), so the failure type is
// only available from the {"error":{"type":...}} body envelope via Type().
// Per https://docs.anthropic.com/en/api/errors:
//   - rate_limit_error → 429
//   - api_error        → 500
//   - overloaded_error → 529
//
// Returns -1 when StatusCode is a real (non-200) HTTP status — those are
// authoritative and the body type must not override them — or when the body
// type isn't one of the three above, so the caller falls back to StatusCode
// rather than inventing a code.
func inStreamErrorCode(apiErr *anthropic.Error) int {
	if apiErr.StatusCode != http.StatusOK && apiErr.StatusCode != 0 {
		return -1
	}
	switch apiErr.Type() {
	case anthropic.ErrorTypeRateLimitError:
		return http.StatusTooManyRequests
	case anthropic.ErrorTypeAPIError:
		return http.StatusInternalServerError
	case anthropic.ErrorTypeOverloadedError:
		return 529 // Anthropic's overloaded_error code; net/http has no constant for it.
	}
	return -1
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

// isRetryableClaudeError reports whether err is a transient Claude API
// failure worth retrying. *anthropic.Error.StatusCode is treated as
// authoritative when it reflects a real failure, with no fall-through to
// keyword matching. The exception is StatusCode 200 (or 0): mid-stream
// failures arrive as SSE error events on an already-open stream, so the SDK
// stamps them with the 200 of the successful stream open — for those the body
// error type decides via inStreamErrorCode, and every code it recognises
// (429, 500, 529) is transient per Anthropic's docs. That deliberately
// includes api_error/500, matching the string fallback below, while
// transport-level 500s stay non-retryable.
func isRetryableClaudeError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		if inStreamErrorCode(apiErr) > 0 {
			return true
		}
		switch apiErr.StatusCode {
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
