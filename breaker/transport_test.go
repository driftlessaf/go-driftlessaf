/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package breaker

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// get issues a GET expected to fail, closing any response body.
func get(t *testing.T, client *http.Client, url string) error {
	t.Helper()
	resp, err := client.Get(url)
	if resp != nil {
		resp.Body.Close()
	}
	return err
}

func TestTransportClassification(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		wantTransient bool
	}{
		{"server_error_is_transient", http.StatusInternalServerError, true},
		{"unavailable_is_transient", http.StatusServiceUnavailable, true},
		{"rate_limit_is_transient", http.StatusTooManyRequests, true},
		{"rekor_batch_cancel_passes_through", 499, false},
		{"ok_passes_through", http.StatusOK, false},
		{"not_found_passes_through", http.StatusNotFound, false},
		{"forbidden_passes_through", http.StatusForbidden, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			client := &http.Client{Transport: NewTransport(nil)}
			resp, err := client.Get(srv.URL)

			var berr *Error
			if tc.wantTransient {
				require.Error(t, err)
				require.True(t, errors.As(err, &berr), "transient status should surface as *Error")
				require.Equal(t, tc.status, berr.StatusCode)
				require.Positive(t, berr.RetryAfter)
				return
			}
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, tc.status, resp.StatusCode)
		})
	}
}

func TestTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // close so the connection is refused

	client := &http.Client{Transport: NewTransport(nil)}
	err := get(t, client, srv.URL)

	var berr *Error
	require.True(t, errors.As(err, &berr), "transport error should surface as *Error")
	require.Zero(t, berr.StatusCode)
	require.Positive(t, berr.RetryAfter)
	require.Error(t, berr.Err)
}

func TestTransportShortCircuits(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewTransport(nil)}
	for range DefaultFailureThreshold {
		require.Error(t, get(t, client, srv.URL))
	}
	require.EqualValues(t, DefaultFailureThreshold, requests.Load())

	// The circuit is open: further requests never reach the server.
	err := get(t, client, srv.URL)
	var berr *Error
	require.True(t, errors.As(err, &berr))
	require.Zero(t, berr.StatusCode, "open circuit should not issue a request")
	require.EqualValues(t, DefaultFailureThreshold, requests.Load())
}

func TestTransportFilter(t *testing.T) {
	var guardedRequests atomic.Int64
	var otherRequests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/guarded" {
			guardedRequests.Add(1)
		} else {
			otherRequests.Add(1)
		}
		w.WriteHeader(499)
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewTransport(nil,
		WithRequestFilter(func(r *http.Request) bool { return r.URL.Path == "/guarded" }),
		WithAdditionalResponseFailure(func(_ *http.Request, resp *http.Response) bool { return resp.StatusCode == 499 }),
		WithFailureThreshold(1),
	)}
	require.Error(t, get(t, client, srv.URL+"/guarded"))
	require.Error(t, get(t, client, srv.URL+"/guarded"))
	require.EqualValues(t, 1, guardedRequests.Load(), "second guarded request should be short-circuited")

	resp, err := client.Get(srv.URL + "/other")
	require.NoError(t, err)
	resp.Body.Close()
	require.EqualValues(t, 1, otherRequests.Load(), "unmatched request should pass through")
}

func TestTransportSuccessCloses(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusServiceUnavailable)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewTransport(nil)}
	for range DefaultFailureThreshold - 1 {
		require.Error(t, get(t, client, srv.URL))
	}

	// A success clears the failure count before the circuit trips.
	status.Store(http.StatusOK)
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	resp.Body.Close()

	status.Store(http.StatusServiceUnavailable)
	for range DefaultFailureThreshold - 1 {
		var berr *Error
		require.True(t, errors.As(get(t, client, srv.URL), &berr))
		require.NotZero(t, berr.StatusCode, "circuit should stay closed below the threshold")
	}
}
