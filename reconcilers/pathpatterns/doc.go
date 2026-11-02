/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package pathpatterns maps repository file paths to reconciler keys with
// include and exclude regular expressions.
//
// Every pattern is anchored at both ends, so it must match the whole path.
// Each include pattern has exactly one capture group, and the group's text is
// the key. A path that any exclude pattern matches has no key. Otherwise the
// first include pattern that matches gives the key, and an empty capture is
// no match.
//
// Path reconcilers share this package so that the same patterns select the
// same paths and produce the same keys whichever reconciler runs them.
package pathpatterns
