/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package telemetry

import (
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
)

func TestResponseCodeAttr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		code int
		want string
	}{
		{name: "0 is success", code: 0, want: "200"},
		{name: "negative is unknown", code: -1, want: "unknown"},
		{name: "200 in-stream error with unrecognised body type", code: 200, want: "200"},
		{name: "429", code: 429, want: "429"},
		{name: "500", code: 500, want: "500"},
		{name: "503", code: 503, want: "503"},
		{name: "529", code: 529, want: "529"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := responseCodeAttr(tt.code); got != tt.want {
				t.Errorf("responseCodeAttr(%d) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

// TestWithAPIRequestCounter_PreservesBaseCallback locks in the composition
// behaviour of WithAPIRequestCounter: it must wrap the existing
// OnAttemptError, not overwrite it. Per-attempt trace recording relies on
// the original callback continuing to fire, so a future edit that drops the
// chaining would silently break llmTurn error capture.
func TestWithAPIRequestCounter_PreservesBaseCallback(t *testing.T) {
	t.Parallel()

	r := NewRecorder(metrics.NewGenAI("test"), "gemini-test", "gcp.vertex_ai", nil, func(error) int { return -1 })
	var got []error
	cfg := retry.RetryConfig{
		OnAttemptError: func(err error) { got = append(got, err) },
	}
	wrapped := r.WithAPIRequestCounter(t.Context(), cfg)

	sentinel := errors.New("transient")
	wrapped.OnAttemptError(sentinel)

	if len(got) != 1 || !errors.Is(got[0], sentinel) {
		t.Errorf("base OnAttemptError not invoked: got %v, want one call with sentinel", got)
	}
}

// TestWithAPIRequestCounter_NilBase ensures the wrapper handles configs
// that don't set OnAttemptError (the outer retry call site uses one). It
// must not panic and must still record the API request.
func TestWithAPIRequestCounter_NilBase(t *testing.T) {
	t.Parallel()

	r := NewRecorder(metrics.NewGenAI("test"), "gemini-test", "gcp.vertex_ai", nil, func(error) int { return -1 })
	cfg := retry.RetryConfig{} // OnAttemptError is nil
	wrapped := r.WithAPIRequestCounter(t.Context(), cfg)

	// Must not panic.
	wrapped.OnAttemptError(errors.New("boom"))
}
