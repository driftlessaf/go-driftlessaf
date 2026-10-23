/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package testing_test

import (
	"fmt"
)

// Example_new demonstrates the intended usage of New.
//
// New requires chainctl to be installed and authenticated. It is intended for
// integration tests that run with the withauth build tag.
func Example_new() {
	// In an integration test, call:
	//
	//   mgr, err := smtesting.New[MyDetails](ctx, t)
	//   if err != nil {
	//       t.Fatal(err)
	//   }
	//
	// The manager is authenticated via chainctl and ready to write attestations.
	fmt.Println("New requires chainctl authentication")
	// Output: New requires chainctl authentication
}

// Example_localSigning shows managers sharing a local trust environment.
func Example_localSigning() {
	// In a test:
	// local := smtesting.NewLocal(t)
	// writer, err := statusmanager.New[Details](t.Context(), local.Options()...)
	// reader, err := statusmanager.NewReadOnly[Details](t.Context(), local.Options()...)
	fmt.Println("local keys and HTTP services; production signer and verifier")
	// Output: local keys and HTTP services; production signer and verifier
}
