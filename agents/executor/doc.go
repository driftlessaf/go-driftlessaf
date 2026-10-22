/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package executor holds the contract shared by the agent executors
// (claudeexecutor, googleexecutor, chatcompletionexecutor): the conditions every
// executor reports the same way, so a caller can handle them without knowing
// which provider ran the agent.
//
// Every executor also records the network timing of each model call on its
// trace turn (agenttrace.RecordedTurn.Latency) and the Region the process runs
// in on the trace (agenttrace.Trace.ClientRegion). A BigQuery recorder that
// rejects unknown keys needs the turns.latency and client_region columns of
// agenttrace's schema before it receives these traces. To stop recording both
// for a process, set DRIFTLESSAF_TRACE_CALL_LATENCY=false. It is read once per
// process, so a change takes effect when the process restarts.
package executor
