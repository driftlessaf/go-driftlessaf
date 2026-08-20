/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package gcs is a Google Cloud Storage backend for the blob content store.
//
// [Store] implements the blob backend method set — Put, Get, Delete, List —
// over a single [storage.BucketHandle], mapping GCS's conditional-write and
// error semantics onto the [blob] contract: object generations are the
// compare-and-swap tokens, and not-found / precondition-failed responses map to
// [blob.ErrNotExist] and [blob.ErrPreconditionFailed].
//
// It returns a concrete *Store; consumers declare the small interface they need
// (see the blobtest package's Store for the full method set).
package gcs
