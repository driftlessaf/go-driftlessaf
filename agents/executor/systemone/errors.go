/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package systemone

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var (
	// ErrInvalidRequest identifies a request rejected before it was sent.
	ErrInvalidRequest = errors.New("invalid system one request")
	// ErrResponseValidation identifies a well-formed HTTP success whose body
	// does not satisfy the API contract or the questions as sent.
	ErrResponseValidation = errors.New("system one response failed validation")
	// ErrResponseTooLarge identifies a response body above the client's cap.
	ErrResponseTooLarge = errors.New("system one response exceeds size cap")
)

// StatusOverloaded is the non-standard status the API returns while
// temporarily overloaded; net/http has no constant for it.
const StatusOverloaded = 529

// APIError is a non-2xx response from the API.
type APIError struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Body is a bounded, control-character-free excerpt of the response body.
	Body string
	// RetryAfter is the server's Retry-After hint, or zero when absent.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := "system one: HTTP " + strconv.Itoa(e.StatusCode)
	if text := statusText(e.StatusCode); text != "" {
		msg += " " + text
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Retryable reports whether the status is one the API documents as
// transient: rate limiting, overload, request timeout, and server errors.
func (e *APIError) Retryable() bool {
	switch {
	case e.StatusCode == http.StatusTooManyRequests,
		e.StatusCode == http.StatusRequestTimeout,
		e.StatusCode >= http.StatusInternalServerError:
		return true
	default:
		return false
	}
}

// statusText names a status, including the API's non-standard 529.
func statusText(code int) string {
	if code == StatusOverloaded {
		return "Overloaded"
	}
	return http.StatusText(code)
}

// IsRetryable reports whether err is a transient failure worth another
// attempt: a retryable [APIError], an HTTP client or transport timeout, or
// another network timeout. Validation failures and client errors are never
// retryable, nor is a bare context error. net/http reports its own timeouts
// as errors that also match context.DeadlineExceeded, so the transport error
// is classified by its Timeout method before the context sentinels are
// consulted; [Client.Ask] separately stops retrying once the caller's
// context is done.
func IsRetryable(err error) bool {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Retryable()
	}
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Timeout()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if netErr, ok := errors.AsType[net.Error](err); ok {
		return netErr.Timeout()
	}
	return false
}

// responseCode maps an attempt outcome to the HTTP-style code recorded in the
// genai.api.requests counter: 0 for success, the status of an APIError, and -1
// for errors that carry no status.
func responseCode(err error) int {
	if err == nil {
		return 0
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.StatusCode
	}
	return -1
}

// bodyExcerptLimit bounds the response text carried on an APIError.
const bodyExcerptLimit = 512

// bodyExcerpt renders an error body for a log-safe message: control and
// format runes become spaces and the result is capped.
func bodyExcerpt(body []byte) string {
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, string(body))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > bodyExcerptLimit {
		s = s[:bodyExcerptLimit] + "..."
	}
	return s
}
