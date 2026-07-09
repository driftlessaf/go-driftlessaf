/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package kmsseal

import "testing"

// TestNewRejectsEmptyKeyName covers the input-validation path that needs no
// GCP: an empty keyName is rejected before any KMS client is constructed, and
// no encryptor or close func leaks out on the error return.
func TestNewRejectsEmptyKeyName(t *testing.T) {
	enc, closeFn, err := New(t.Context(), "")
	if err == nil {
		t.Fatal("New accepted an empty keyName")
	}
	if enc != nil {
		t.Errorf("New returned a non-nil encryptor on error: %v", enc)
	}
	if closeFn != nil {
		t.Error("New returned a non-nil close func on error")
	}
}
