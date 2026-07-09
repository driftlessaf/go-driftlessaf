/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package transient provides shared handling of transient upstream failures
// for reconcilers: Retry absorbs short blips in-process and marks errors it
// can't, and Is detects marked errors and temporary registry errors, so
// frameworks can requeue them instead of failing hard.
package transient
