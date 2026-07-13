/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import "sync"

// localSpanSink is a process-global per-turn span sink invoked by LLMTurn.End
// for every completed turn, in addition to (and independently of) the
// per-tracer spanEmitter. It exists for in-process consumers that run many
// differently-typed agents in one process and therefore cannot register a
// per-response-type Tracer[T]: the Tracer is generic over the response type and
// its spanEmitter is set per NewTrace, so there is no single context install
// that reaches every agent. A CLI writing one local telemetry file is the
// canonical case. This mirrors the ecosystems vmtool.SetResultSink pattern —
// a package-global, no-op by default, restored via a returned closure.
var (
	localSpanSinkMu sync.RWMutex
	localSpanSink   SpanEmitter
)

// SetLocalSpanSink installs fn as the process-global span sink. LLMTurn.End
// calls it for every turn that recorded a request payload (i.e. under
// WithPayloadsEnabled(ctx, true) — without that opt-in buildRecordedSpan yields
// nothing and the sink is never called), regardless of the context tracer or
// its response type. It fires in ADDITION to any per-tracer spanEmitter, so a
// consumer that installs both receives each span twice; typically only one is
// in use (the CloudEvent emitter in production, or this local sink in a CLI).
//
// fn must be non-blocking and safe for concurrent use: End invokes it
// synchronously on the turn-cleanup path, and turns from concurrently-executing
// agents can call it in parallel. Passing nil clears the sink. The returned
// restore func reinstalls the previously-registered sink — defer it to scope a
// sink to one run rather than leaking a file handle into the process global.
func SetLocalSpanSink(fn SpanEmitter) (restore func()) {
	localSpanSinkMu.Lock()
	prev := localSpanSink
	localSpanSink = fn
	localSpanSinkMu.Unlock()
	return func() {
		localSpanSinkMu.Lock()
		localSpanSink = prev
		localSpanSinkMu.Unlock()
	}
}

// currentLocalSpanSink returns the installed global span sink, or nil.
func currentLocalSpanSink() SpanEmitter {
	localSpanSinkMu.RLock()
	defer localSpanSinkMu.RUnlock()
	return localSpanSink
}
