/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package breaker

import (
	"fmt"
	"net/http"
	"time"
)

// Error is a transient failure reported by Transport: an open circuit, a
// transport-level error, or a transient status code (5xx/429).
type Error struct {
	// Key identifies the circuit: the request's host.
	Key string
	// StatusCode is the transient status code received, or zero when no
	// response was received (open circuit or transport error).
	StatusCode int
	// RetryAfter is the backoff to wait before retrying the host, suited to a
	// workqueue.RequeueNotBefore floor.
	RetryAfter time.Duration
	// Err is the underlying transport error, if any.
	Err error
}

func (e *Error) Error() string {
	switch {
	case e.Err != nil:
		return fmt.Sprintf("%s: retry after %s: %v", e.Key, e.RetryAfter, e.Err)
	case e.StatusCode != 0:
		return fmt.Sprintf("%s: unexpected status code %d, retry after %s", e.Key, e.StatusCode, e.RetryAfter)
	default:
		return fmt.Sprintf("%s: circuit open, retry after %s", e.Key, e.RetryAfter)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// Transport is an http.RoundTripper guarding requests with a per-host Breaker.
// Transport-level failures and transient status codes (5xx/429) count against
// the host's circuit; any other response (including 4xx) closes it. Once open,
// requests short-circuit without reaching the network until a half-open probe
// succeeds.
//
// Unlike a conventional RoundTripper, transient-status responses are consumed
// and returned as a *Error so every failure carries its backoff.
type Transport struct {
	base    http.RoundTripper
	breaker *Breaker
}

var _ http.RoundTripper = (*Transport)(nil)

// NewTransport returns a Transport wrapping base (http.DefaultTransport when
// nil) with a Breaker configured by opts.
func NewTransport(base http.RoundTripper, opts ...Option) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{base: base, breaker: New(opts...)}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	if ok, retryAfter := t.breaker.Allow(host); !ok {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, &Error{Key: host, RetryAfter: retryAfter}
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, &Error{Key: host, RetryAfter: t.breaker.RecordFailure(host), Err: err}
	}
	if resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		return nil, &Error{Key: host, StatusCode: resp.StatusCode, RetryAfter: t.breaker.RecordFailure(host)}
	}
	t.breaker.RecordSuccess(host)
	return resp, nil
}
