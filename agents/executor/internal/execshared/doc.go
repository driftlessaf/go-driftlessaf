/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package execshared holds behavior shared by the executor implementations
// (claudeexecutor, googleexecutor, chatcompletionexecutor) so each backend applies
// identical semantics: prompt-suffix handling, resource-label defaults, the
// submit routing predicate, the bounded tool-call dispatch pool, the
// submit-gate validation tail, and recording each model call's latency and
// the process's client Region on the trace unless
// DRIFTLESSAF_TRACE_CALL_LATENCY=false turns that off.
package execshared
