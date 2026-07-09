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
