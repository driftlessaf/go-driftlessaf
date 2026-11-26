/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package conformance provides a suite of conformance tests for workqueue
// implementations.
//
// Use TestSemantics and TestConcurrency to verify that a workqueue.Interface
// implementation satisfies the expected behavioral contracts. A durable
// implementation that leases in-progress keys also runs TestLeases.
package conformance
