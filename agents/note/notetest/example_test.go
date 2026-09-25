/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package notetest_test

import (
	"fmt"
	"testing"

	"chainguard.dev/driftlessaf/agents/note"
	"chainguard.dev/driftlessaf/agents/note/notetest"
)

// ExampleRunConformance shows how to wire a backend into the conformance suite.
// This is a compile-only example: a TestConformance function supplies the
// *testing.T when running it. Each factory call must return a fresh, empty store;
// a durable backend uses a separate namespace per call.
func ExampleRunConformance() {
	var t *testing.T // supplied by the test runner in TestConformance(t *testing.T)
	notetest.RunConformance(t, func() note.Store { return note.NewMem() })
}

// ExampleSortNotes orders notes by key, run, name, and author without changing
// the input slice. This lets backend tests compare listings with different orders.
func ExampleSortNotes() {
	notes := []note.Note{
		{Key: "harden:widget", Run: 2, Name: "harden/fix_plan", Author: "claude"},
		{Key: "harden:widget", Run: 1, Name: "review/critique", Author: "claude"},
		{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "gemini"},
		{Key: "harden:gadget", Run: 2, Name: "harden/fix_plan", Author: "claude"},
		{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "claude"},
	}
	for _, n := range notetest.SortNotes(notes) {
		fmt.Println(n.Key, n.Run, n.Name, n.Author)
	}
	fmt.Printf("original first note: %+v\n", notes[0])
	// Output:
	// harden:gadget 2 harden/fix_plan claude
	// harden:widget 1 harden/fix_plan claude
	// harden:widget 1 harden/fix_plan gemini
	// harden:widget 1 review/critique claude
	// harden:widget 2 harden/fix_plan claude
	// original first note: {Key:harden:widget Run:2 Name:harden/fix_plan Author:claude}
}
