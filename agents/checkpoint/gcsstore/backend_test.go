/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstore

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	"chainguard.dev/driftlessaf/agents/checkpoint"
)

// TestMapDeleteErr pins the translation of real GCS client errors into the
// contract that makes Delete a claim-once CAS: stale-generation (HTTP 412) and
// already-deleted (HTTP 404 / storage.ErrObjectNotExist) — including wrapped
// forms — surface as checkpoint.ErrTokenMismatch, while anything else passes
// through untouched so callers can retry transient failures.
func TestMapDeleteErr(t *testing.T) {
	serverErr := &googleapi.Error{Code: http.StatusServiceUnavailable, Message: "backend unavailable"}
	opaqueErr := errors.New("connection reset")

	tests := []struct {
		name string
		err  error
		want error
	}{{
		name: "nil passes through",
		err:  nil,
		want: nil,
	}, {
		name: "sentinel not-exist is a token mismatch",
		err:  storage.ErrObjectNotExist,
		want: checkpoint.ErrTokenMismatch,
	}, {
		name: "http 404 is a token mismatch",
		err:  &googleapi.Error{Code: http.StatusNotFound},
		want: checkpoint.ErrTokenMismatch,
	}, {
		name: "http 412 is a token mismatch",
		err:  &googleapi.Error{Code: http.StatusPreconditionFailed},
		want: checkpoint.ErrTokenMismatch,
	}, {
		name: "wrapped 404 is a token mismatch",
		err:  fmt.Errorf("deleting checkpoint: %w", &googleapi.Error{Code: http.StatusNotFound}),
		want: checkpoint.ErrTokenMismatch,
	}, {
		name: "wrapped 412 is a token mismatch",
		err:  fmt.Errorf("deleting checkpoint: %w", &googleapi.Error{Code: http.StatusPreconditionFailed}),
		want: checkpoint.ErrTokenMismatch,
	}, {
		name: "server error passes through",
		err:  serverErr,
		want: serverErr,
	}, {
		name: "opaque error passes through",
		err:  opaqueErr,
		want: opaqueErr,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapDeleteErr(tt.err)
			if !errors.Is(got, tt.want) {
				t.Errorf("mapDeleteErr: got = %v, want = %v", got, tt.want)
			}
			// The mismatch sentinel must come back unwrapped (not inside a
			// wrapper chain) so the Store.Delete contract holds exactly.
			if errors.Is(tt.want, checkpoint.ErrTokenMismatch) && errors.Unwrap(got) != nil {
				t.Errorf("mapDeleteErr: got wrapped %v, want the bare sentinel %v", got, checkpoint.ErrTokenMismatch)
			}
		})
	}
}
