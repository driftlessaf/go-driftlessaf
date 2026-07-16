/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package execshared holds behavior shared by the executor implementations
// (claudeexecutor, googleexecutor, openaiexecutor) so each backend applies
// identical semantics: prompt-suffix handling, resource-label defaults, the
// submit routing predicate, the bounded tool-call dispatch pool, and the
// submit-gate validation tail.
package execshared
