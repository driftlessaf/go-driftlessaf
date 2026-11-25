/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package schedtest holds the conformance suite for dag schedulers and a
// serial reference scheduler that passes it.
//
// The concurrent scheduler in package dag and [Serial] decide the same things
// (what runs, what is skipped, what waits, what resumes) by different means.
// Both run [RunConformance], so a rule changed in one and not the other fails.
package schedtest
