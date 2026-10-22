/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package calltiming

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
)

// Timer collects the network timing of one model call. Create one per
// executor-level attempt and pass Context's result to the SDK call. The zero
// value is ready to use.
//
// Each new HTTP request on the context (GetConn) resets the Timer. After an
// SDK-internal retry, or a request made before the model call such as a
// credential refresh, the timing therefore describes the last request: the one
// that returned the response.
//
// Timer is safe for concurrent use: the transport calls the hooks from its own
// goroutines while the executor reads the response stream.
type Timer struct {
	// now is the clock; nil means time.Now.
	now func() time.Time

	mu  sync.Mutex
	req request
}

// request is the timing of one HTTP request.
type request struct {
	// start is when the request asked for a connection.
	start time.Time
	// gotConn is set once the request holds a connection. Dial and handshake
	// events after it belong to a connection the request did not use, such as
	// a dial that lost the race to an idle connection.
	gotConn bool
	reused  bool
	// dials holds ConnectStart times by network and address: with dual-stack
	// dialing, several dials can race and only one completes.
	dials    map[string]time.Time
	connect  *time.Duration
	tlsStart time.Time
	tls      *time.Duration
	// wrote, firstByte, and firstToken are when the request was fully
	// written, the first response byte arrived, and the first streamed token
	// arrived.
	wrote      time.Time
	firstByte  time.Time
	firstToken time.Time
}

// timerKey is the context key under which Context stores the Timer.
type timerKey struct{}

// Context returns ctx with t's httptrace hooks attached, composed with any
// hooks ctx already carries. FromContext on the result returns t.
func (t *Timer) Context(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, timerKey{}, t)
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn:              t.getConn,
		ConnectStart:         t.connectStart,
		ConnectDone:          t.connectDone,
		TLSHandshakeStart:    t.tlsHandshakeStart,
		TLSHandshakeDone:     t.tlsHandshakeDone,
		GotConn:              t.gotConnection,
		WroteRequest:         t.wroteRequest,
		GotFirstResponseByte: t.gotFirstResponseByte,
	})
}

// FromContext returns the Timer that Context attached to ctx, or nil.
func FromContext(ctx context.Context) *Timer {
	t, _ := ctx.Value(timerKey{}).(*Timer)
	return t
}

// MarkFirstToken records that the first output token of the current request
// arrived. Later calls for the same request are ignored.
func (t *Timer) MarkFirstToken() {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.req.firstToken.IsZero() {
		t.req.firstToken = now
	}
}

// Latency returns the timing of the last HTTP request, or the zero value when
// no request was observed.
func (t *Timer) Latency() agenttrace.TurnLatency {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.req
	if r.start.IsZero() {
		return agenttrace.TurnLatency{}
	}
	l := agenttrace.TurnLatency{RequestStartTime: r.start.UTC()}
	if r.gotConn {
		l.ConnReused = new(r.reused)
		if !r.reused {
			l.ConnectMS = millis(r.connect)
			l.TLSMS = millis(r.tls)
		}
	}
	if !r.wrote.IsZero() {
		l.FirstByteMS = elapsed(r.wrote, r.firstByte)
		l.FirstTokenMS = elapsed(r.wrote, r.firstToken)
	}
	return l
}

func (t *Timer) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *Timer) getConn(string) {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.req = request{start: now}
}

func (t *Timer) connectStart(network, addr string) {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.req.gotConn {
		return
	}
	if t.req.dials == nil {
		t.req.dials = make(map[string]time.Time, 1)
	}
	t.req.dials[network+" "+addr] = now
}

func (t *Timer) connectDone(network, addr string, err error) {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.req.gotConn || err != nil || t.req.connect != nil {
		return
	}
	if start, ok := t.req.dials[network+" "+addr]; ok {
		t.req.connect = new(now.Sub(start))
	}
}

func (t *Timer) tlsHandshakeStart() {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.req.gotConn {
		t.req.tlsStart = now
	}
}

func (t *Timer) tlsHandshakeDone(_ tls.ConnectionState, err error) {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.req.gotConn || err != nil || t.req.tlsStart.IsZero() || t.req.tls != nil {
		return
	}
	t.req.tls = new(now.Sub(t.req.tlsStart))
}

func (t *Timer) gotConnection(info httptrace.GotConnInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.req.gotConn = true
	t.req.reused = info.Reused
}

func (t *Timer) wroteRequest(info httptrace.WroteRequestInfo) {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if info.Err == nil {
		t.req.wrote = now
	}
}

func (t *Timer) gotFirstResponseByte() {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.req.firstByte.IsZero() {
		t.req.firstByte = now
	}
}

// millis converts an observed duration to whole milliseconds.
func millis(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	return new(d.Milliseconds())
}

// elapsed returns the whole milliseconds from wrote to at, or nil when at was
// not observed or came first. A server can answer before the request is fully
// written; that response has no write-to-response duration.
func elapsed(wrote, at time.Time) *int64 {
	if at.IsZero() || at.Before(wrote) {
		return nil
	}
	return new(at.Sub(wrote).Milliseconds())
}
