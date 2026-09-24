/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package notetest_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/note"
	"chainguard.dev/driftlessaf/agents/note/notetest"
)

// ExampleRunConformance shows the constructor shape RunConformance expects. The
// suite itself needs a *testing.T, so a backend wires it into a regular test:
//
//	func TestConformance(t *testing.T) {
//		notetest.RunConformance(t, func() note.Store { return note.NewMem() })
//	}
//
// A durable backend hands back a store over a fresh namespace per call, so the
// suite's key-wildcard listings see only the notes it stored:
//
//	notetest.RunConformance(t, func() note.Store {
//		root := fmt.Sprintf("notetest/%d/", time.Now().UnixNano())
//		return gcsstore.New(root, gcs.New(bucket))
//	})
func ExampleRunConformance() {
	// RunConformance calls the constructor once per subtest; each call must
	// return a store over a fresh, empty namespace.
	newStore := func() note.Store { return note.NewMem() }

	// The suite entry point only executes under a *testing.T (see above).
	suite := notetest.RunConformance
	fmt.Println("suite wired:", suite != nil)
	fmt.Println("fresh store ready:", newStore() != nil)
	// Output:
	// suite wired: true
	// fresh store ready: true
}

// ExampleSortNotes normalizes a listing's order for comparison without changing
// the original slice.
func ExampleSortNotes() {
	notes := []note.Note{
		{Key: "harden:widget", Run: 2, Name: "fix_plan", Author: "claude"},
		{Key: "harden:widget", Run: 1, Name: "fix_plan", Author: "gemini"},
		{Key: "harden:widget", Run: 1, Name: "fix_plan", Author: "claude"},
	}
	for _, n := range notetest.SortNotes(notes) {
		fmt.Printf("run %d: %s\n", n.Run, n.Author)
	}
	fmt.Println("original first run:", notes[0].Run)
	// Output:
	// run 1: claude
	// run 1: gemini
	// run 2: claude
	// original first run: 2
}
