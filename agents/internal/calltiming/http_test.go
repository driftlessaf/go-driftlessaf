/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package calltiming_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/internal/calltiming"
)

// get sends one request on a context carrying timer and drains the response.
func get(t *testing.T, ctx context.Context, client *http.Client, url string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
}

// timedGet sends one request under a fresh Timer and returns its timing.
func timedGet(t *testing.T, client *http.Client, url string) agenttrace.TurnLatency {
	t.Helper()
	timer := new(calltiming.Timer)
	get(t, timer.Context(t.Context()), client, url)
	got := timer.Latency()
	if err := got.Validate(); err != nil {
		t.Fatalf("Latency().Validate(): %v", err)
	}
	return got
}

func TestTimerOverHTTP(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	tests := []struct {
		name    string
		start   func(*httptest.Server)
		wantTLS bool
	}{
		{name: "HTTP/1.1", start: (*httptest.Server).Start},
		{name: "HTTP/1.1 over TLS", start: (*httptest.Server).StartTLS, wantTLS: true},
		{name: "HTTP/2", start: func(s *httptest.Server) {
			s.EnableHTTP2 = true
			s.StartTLS()
		}, wantTLS: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewUnstartedServer(ok)
			tt.start(srv)
			t.Cleanup(srv.Close)
			client := srv.Client()

			first := timedGet(t, client, srv.URL)
			if first.RequestStartTime.IsZero() {
				t.Error("first request: RequestStartTime is zero")
			}
			if first.ConnReused == nil || *first.ConnReused {
				t.Errorf("first request: ConnReused = %v, want false", first.ConnReused)
			}
			if first.ConnectMS == nil {
				t.Error("first request: ConnectMS = nil, want the dial time")
			}
			if got := first.TLSMS != nil; got != tt.wantTLS {
				t.Errorf("first request: TLSMS set = %t, want %t", got, tt.wantTLS)
			}
			if first.FirstByteMS == nil {
				t.Error("first request: FirstByteMS = nil, want the time to first byte")
			}
			if first.FirstTokenMS != nil {
				t.Errorf("first request: FirstTokenMS = %d, want nil when no token was marked", *first.FirstTokenMS)
			}

			second := timedGet(t, client, srv.URL)
			if second.ConnReused == nil || !*second.ConnReused {
				t.Errorf("second request: ConnReused = %v, want true", second.ConnReused)
			}
			if second.ConnectMS != nil || second.TLSMS != nil {
				t.Errorf("second request: ConnectMS = %v, TLSMS = %v, want nil on a reused connection", second.ConnectMS, second.TLSMS)
			}
			if second.FirstByteMS == nil {
				t.Error("second request: FirstByteMS = nil, want the time to first byte")
			}
		})
	}
}

// An SDK that retries inside one executor attempt sends several requests on
// the same context. The timing must describe the request that answered.
func TestTimerKeepsTheAnsweringRequest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	t.Cleanup(srv.Close)
	client := srv.Client()

	timer := new(calltiming.Timer)
	ctx := timer.Context(t.Context())
	get(t, ctx, client, srv.URL)
	timer.MarkFirstToken()
	first := timer.Latency()

	get(t, ctx, client, srv.URL)
	got := timer.Latency()
	if !got.RequestStartTime.After(first.RequestStartTime) {
		t.Errorf("RequestStartTime: got = %v, want after the first request's %v", got.RequestStartTime, first.RequestStartTime)
	}
	if got.ConnReused == nil || !*got.ConnReused {
		t.Errorf("ConnReused: got = %v, want true for the answering request", got.ConnReused)
	}
	if got.ConnectMS != nil || got.TLSMS != nil {
		t.Errorf("ConnectMS = %v, TLSMS = %v: want nil, the first request's setup must not carry over", got.ConnectMS, got.TLSMS)
	}
	if got.FirstTokenMS != nil {
		t.Errorf("FirstTokenMS: got = %d, want nil, the first request's token must not carry over", *got.FirstTokenMS)
	}
}

// Hooks already on the context keep running: Context composes with them.
func TestTimerComposesWithCallerTrace(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	t.Cleanup(srv.Close)

	var callerSaw atomic.Bool
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotFirstResponseByte: func() { callerSaw.Store(true) }})
	timer := new(calltiming.Timer)
	get(t, timer.Context(ctx), srv.Client(), srv.URL)
	if !callerSaw.Load() {
		t.Error("caller's GotFirstResponseByte hook did not run")
	}
	if timer.Latency().FirstByteMS == nil {
		t.Error("timer FirstByteMS = nil, want set")
	}
}
