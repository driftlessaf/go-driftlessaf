/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package storetest_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/memstore"
	"chainguard.dev/driftlessaf/agents/checkpoint/storetest"
)

// ExampleRunConformance demonstrates the constructor shape RunConformance
// expects. The suite itself needs a *testing.T, so a Store package wires it
// into a regular test:
//
//	func TestConformance(t *testing.T) {
//		storetest.RunConformance(t, func() checkpoint.Store {
//			return memstore.New()
//		})
//	}
func ExampleRunConformance() {
	// RunConformance calls the constructor once per subtest; each call must
	// return a fresh, empty Store.
	newStore := func() checkpoint.Store { return memstore.New() }

	// The suite entry point only executes under a *testing.T (see above).
	suite := storetest.RunConformance
	fmt.Println("suite wired:", suite != nil)
	fmt.Println("fresh store ready:", newStore() != nil)
	// Output:
	// suite wired: true
	// fresh store ready: true
}
