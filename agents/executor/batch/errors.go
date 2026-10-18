/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/api/googleapi"
	"google.golang.org/genai"
)

var (
	// ErrInvalidRequest identifies a request the client rejects: invalid
	// input, or an id whose existing job does not match this client.
	ErrInvalidRequest = errors.New("invalid batch request")
	// ErrRefused marks a result the provider's safety system declined to
	// answer. [Result.Refusal] carries the provider's category and detail.
	ErrRefused = errors.New("batch request refused")
	// ErrTruncated marks a result that stopped at its output token limit.
	ErrTruncated = errors.New("batch response truncated at max tokens")
	// ErrMissing marks a submitted key with no output line once the job
	// reached a terminal state.
	ErrMissing = errors.New("batch result missing")
)

// statusOverloaded is the non-standard status Vertex returns when a
// partner model is overloaded; net/http has no constant for it.
const statusOverloaded = 529

// statusClientClosed is the non-standard status Vertex uses for a cancelled
// request.
const statusClientClosed = 499

// lineError is a provider error reported for one output line.
type lineError struct {
	code    int
	message string
	// transient marks a line that carries no status but is worth resending,
	// such as a request that never ran.
	transient bool
}

func (e *lineError) Error() string {
	if e.code > 0 {
		return fmt.Sprintf("batch line failed (code %d): %s", e.code, e.message)
	}
	return "batch line failed: " + e.message
}

// IsRetryable reports whether err is a transient failure worth another
// attempt: a per-line provider error or an API error with a rate-limit,
// overload, timeout, or server status, or a network timeout. [ErrMissing]
// is not covered; a caller decides separately whether to resubmit a key
// with no output. [ErrInvalidRequest] never is.
func IsRetryable(err error) bool {
	// An invalid request's text can quote a foreign job, whose URIs must
	// not match the status keywords below.
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidRequest) {
		return false
	}
	if e, ok := errors.AsType[*lineError](err); ok && e.transient {
		return true
	}
	switch responseCode(err) {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, statusClientClosed,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, statusOverloaded:
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Timeout()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if netErr, ok := errors.AsType[net.Error](err); ok {
		return netErr.Timeout()
	}
	return false
}

// responseCode maps an error to the HTTP-style code recorded on
// genai.api.requests: 0 for success, the structured status when there is
// one, a code recovered from gRPC keywords in the message, or -1.
func responseCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := errors.AsType[*lineError](err); ok && e.code > 0 {
		return e.code
	}
	if e, ok := errors.AsType[*genai.APIError](err); ok {
		return e.Code
	}
	if e, ok := errors.AsType[genai.APIError](err); ok {
		return e.Code
	}
	if e, ok := errors.AsType[*googleapi.Error](err); ok {
		return e.Code
	}
	return codeFromMessage(err.Error())
}

// codeFromMessage recovers a status from the gRPC keywords and Anthropic
// error types Vertex puts in error text, or returns -1.
func codeFromMessage(s string) int {
	switch {
	case strings.Contains(s, "RESOURCE_EXHAUSTED"), strings.Contains(s, "rate_limit_error"),
		strings.Contains(s, "quota exceeded"):
		return http.StatusTooManyRequests
	case strings.Contains(s, "overloaded_error"), strings.Contains(s, "Overloaded"):
		return statusOverloaded
	case strings.Contains(s, "UNAVAILABLE"):
		return http.StatusServiceUnavailable
	case strings.Contains(s, "DEADLINE_EXCEEDED"), strings.Contains(s, "timeout_error"):
		return http.StatusGatewayTimeout
	case strings.Contains(s, "INTERNAL"), strings.Contains(s, "Internal error"),
		strings.Contains(s, "api_error"):
		return http.StatusInternalServerError
	}
	return -1
}
