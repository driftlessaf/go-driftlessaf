/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package blobtest provides a shared conformance suite for blob backends.
//
// Every blob backend — the in-memory [blob.Mem] and the GCS-backed store alike
// — is driven through the same [RunConformance] table, so an in-memory double
// cannot silently diverge from the real implementation: the moment a backend
// stops matching the contract, its conformance test fails.
package blobtest
