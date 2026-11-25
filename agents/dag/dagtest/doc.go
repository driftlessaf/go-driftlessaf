/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package dagtest provides an in-memory [dag.Resumer] and a conformance suite
// that every Resumer, durable or not, must pass.
//
// [Mem] stands in for a caller's real Resumer in tests. Both run
// [RunResumerConformance], so the double cannot drift from the store it
// replaces: a Resumer that serves an output under the wrong step or hash, lets
// two keys alias, or keeps a reference to the caller's slice fails the suite.
package dagtest
