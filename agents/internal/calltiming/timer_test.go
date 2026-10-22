/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package calltiming

import (
	"crypto/tls"
	"errors"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"github.com/google/go-cmp/cmp"
)

// fakeClock is a manually advanced clock. The hook-sequence tests call every
// hook from one goroutine, so it needs no lock.
type fakeClock struct{ now time.Time }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// step is one hook call in a scripted request, made after advancing the clock
// by wait.
type step struct {
	wait time.Duration
	call func(*httptrace.ClientTrace, *Timer)
}

func at(wait time.Duration, call func(*httptrace.ClientTrace, *Timer)) step {
	return step{wait: wait, call: call}
}

func getConn(ct *httptrace.ClientTrace, _ *Timer) { ct.GetConn("api.example:443") }
func connectStart(addr string) func(*httptrace.ClientTrace, *Timer) {
	return func(ct *httptrace.ClientTrace, _ *Timer) { ct.ConnectStart("tcp", addr) }
}
func connectDone(addr string, err error) func(*httptrace.ClientTrace, *Timer) {
	return func(ct *httptrace.ClientTrace, _ *Timer) { ct.ConnectDone("tcp", addr, err) }
}
func tlsStart(ct *httptrace.ClientTrace, _ *Timer) { ct.TLSHandshakeStart() }
func tlsDone(err error) func(*httptrace.ClientTrace, *Timer) {
	return func(ct *httptrace.ClientTrace, _ *Timer) { ct.TLSHandshakeDone(tls.ConnectionState{}, err) }
}
func gotConn(reused bool) func(*httptrace.ClientTrace, *Timer) {
	return func(ct *httptrace.ClientTrace, _ *Timer) { ct.GotConn(httptrace.GotConnInfo{Reused: reused}) }
}
func wrote(err error) func(*httptrace.ClientTrace, *Timer) {
	return func(ct *httptrace.ClientTrace, _ *Timer) { ct.WroteRequest(httptrace.WroteRequestInfo{Err: err}) }
}
func firstByte(ct *httptrace.ClientTrace, _ *Timer) { ct.GotFirstResponseByte() }
func firstToken(_ *httptrace.ClientTrace, t *Timer) { t.MarkFirstToken() }

func TestTimerLatency(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	errDial := errors.New("dial failed")
	tests := []struct {
		name  string
		steps []step
		want  agenttrace.TurnLatency
	}{{
		name: "no request observed",
		want: agenttrace.TurnLatency{},
	}, {
		name: "new TLS connection, streamed",
		steps: []step{
			at(0, getConn),
			at(1*time.Millisecond, connectStart("10.0.0.1:443")),
			at(12*time.Millisecond, connectDone("10.0.0.1:443", nil)),
			at(0, tlsStart),
			at(30*time.Millisecond, tlsDone(nil)),
			at(0, gotConn(false)),
			at(2*time.Millisecond, wrote(nil)),
			at(400*time.Millisecond, firstByte),
			at(50*time.Millisecond, firstToken),
			at(5*time.Millisecond, firstToken),
		},
		want: agenttrace.TurnLatency{
			RequestStartTime: base,
			ConnectMS:        new(int64(12)),
			TLSMS:            new(int64(30)),
			ConnReused:       new(false),
			FirstByteMS:      new(int64(400)),
			FirstTokenMS:     new(int64(450)),
		},
	}, {
		name: "reused connection, not streamed",
		steps: []step{
			at(0, getConn),
			at(0, gotConn(true)),
			at(1*time.Millisecond, wrote(nil)),
			at(900*time.Millisecond, firstByte),
		},
		want: agenttrace.TurnLatency{
			RequestStartTime: base,
			ConnReused:       new(true),
			FirstByteMS:      new(int64(900)),
		},
	}, {
		// The SDK retried internally: the second request answered, on the
		// connection the first one opened.
		name: "a new request resets the timer",
		steps: []step{
			at(0, getConn),
			at(0, connectStart("10.0.0.1:443")),
			at(10*time.Millisecond, connectDone("10.0.0.1:443", nil)),
			at(0, gotConn(false)),
			at(0, wrote(nil)),
			at(100*time.Millisecond, firstByte),
			at(time.Second, getConn),
			at(0, gotConn(true)),
			at(0, wrote(nil)),
			at(300*time.Millisecond, firstByte),
		},
		want: agenttrace.TurnLatency{
			RequestStartTime: base.Add(1110 * time.Millisecond),
			ConnReused:       new(true),
			FirstByteMS:      new(int64(300)),
		},
	}, {
		name: "dual-stack dials keep the dial that completed",
		steps: []step{
			at(0, getConn),
			at(0, connectStart("[2001:db8::1]:443")),
			at(5*time.Millisecond, connectStart("10.0.0.1:443")),
			at(20*time.Millisecond, connectDone("[2001:db8::1]:443", errDial)),
			at(3*time.Millisecond, connectDone("10.0.0.1:443", nil)),
			at(0, gotConn(false)),
		},
		want: agenttrace.TurnLatency{
			RequestStartTime: base,
			ConnectMS:        new(int64(23)),
			ConnReused:       new(false),
		},
	}, {
		// A dial that lost the race to an idle connection finishes after the
		// request already holds one; it is not this request's setup cost.
		name: "dial completing after the connection was handed over",
		steps: []step{
			at(0, getConn),
			at(0, connectStart("10.0.0.1:443")),
			at(1*time.Millisecond, gotConn(true)),
			at(10*time.Millisecond, connectDone("10.0.0.1:443", nil)),
			at(0, tlsStart),
			at(10*time.Millisecond, tlsDone(nil)),
		},
		want: agenttrace.TurnLatency{
			RequestStartTime: base,
			ConnReused:       new(true),
		},
	}, {
		name: "failed handshake records no TLS time",
		steps: []step{
			at(0, getConn),
			at(0, tlsStart),
			at(10*time.Millisecond, tlsDone(errors.New("handshake failed"))),
			at(0, gotConn(false)),
		},
		want: agenttrace.TurnLatency{
			RequestStartTime: base,
			ConnReused:       new(false),
		},
	}, {
		name: "failed write records no response times",
		steps: []step{
			at(0, getConn),
			at(0, gotConn(true)),
			at(0, wrote(errors.New("broken pipe"))),
			at(10*time.Millisecond, firstByte),
		},
		want: agenttrace.TurnLatency{
			RequestStartTime: base,
			ConnReused:       new(true),
		},
	}, {
		// roundTrip can hand back the response before the write loop reports
		// the write at all, and Latency can be read in that window.
		name: "first byte before the write is reported",
		steps: []step{
			at(0, getConn),
			at(0, gotConn(true)),
			at(3*time.Millisecond, firstByte),
		},
		want: agenttrace.TurnLatency{
			RequestStartTime: base,
			ConnReused:       new(true),
			FirstByteMS:      new(int64(0)),
		},
	}, {
		// Also the order Linux can report a fast reply in: the read loop sees
		// the first byte before the write loop reports the write.
		name: "response before the request was fully written",
		steps: []step{
			at(0, getConn),
			at(0, gotConn(true)),
			at(0, firstByte),
			at(5*time.Millisecond, wrote(nil)),
		},
		want: agenttrace.TurnLatency{
			RequestStartTime: base,
			ConnReused:       new(true),
			FirstByteMS:      new(int64(0)),
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeClock{now: base}
			timer := &Timer{now: func() time.Time { return clock.now }}
			ct := httptrace.ContextClientTrace(timer.Context(t.Context()))
			for _, s := range tt.steps {
				clock.advance(s.wait)
				s.call(ct, timer)
			}
			got := timer.Latency()
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Latency() mismatch (-want +got):\n%s", diff)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("Latency().Validate(): got = %v, want = nil", err)
			}
		})
	}
}

func TestFromContext(t *testing.T) {
	t.Parallel()
	if got := FromContext(t.Context()); got != nil {
		t.Errorf("FromContext(no timer): got = %p, want = nil", got)
	}
	timer := new(Timer)
	if got := FromContext(timer.Context(t.Context())); got != timer {
		t.Errorf("FromContext: got = %p, want = %p", got, timer)
	}
}

// The transport calls the hooks from its own goroutines while the executor
// marks tokens and reads the result; -race checks the locking.
func TestTimerConcurrentHooks(t *testing.T) {
	t.Parallel()
	timer := new(Timer)
	ct := httptrace.ContextClientTrace(timer.Context(t.Context()))
	ct.GetConn("api.example:443")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ct.ConnectStart("tcp", "10.0.0.1:443")
			ct.ConnectDone("tcp", "10.0.0.1:443", nil)
			ct.TLSHandshakeStart()
			ct.TLSHandshakeDone(tls.ConnectionState{}, nil)
			ct.GotConn(httptrace.GotConnInfo{})
			ct.WroteRequest(httptrace.WroteRequestInfo{})
			ct.GotFirstResponseByte()
			timer.MarkFirstToken()
			_ = timer.Latency()
		})
	}
	wg.Wait()
	if got := timer.Latency(); got.ConnReused == nil || *got.ConnReused {
		t.Errorf("ConnReused: got = %v, want = false", got.ConnReused)
	}
}
