/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package note_test

import (
	"context"
	"fmt"
	"io"
	"strings"

	"chainguard.dev/driftlessaf/agents/note"
)

// ExampleRef shows the content-addressed key: any agent computes the same Ref
// from a note's coordinates, so it can address another agent's note without a
// lookup — and changing any coordinate yields a different key.
func ExampleRef() {
	ref := note.Ref("harden:widget", 1, "fix_plan", "claude")
	same := note.Ref("harden:widget", 1, "fix_plan", "claude")
	otherAuthor := note.Ref("harden:widget", 1, "fix_plan", "gemini")

	fmt.Println("stable:", ref == same)
	fmt.Println("per-author:", ref != otherAuthor)
	// Output:
	// stable: true
	// per-author: true
}

// ExampleMem passes notes through the in-memory store: two authors each Put their
// fix_plan for one key+run, a consumer Gets one back by its coordinates, and List
// gathers every author's fix_plan for that run.
func ExampleMem() {
	ctx := context.Background()
	store := note.NewMem()

	run := 1
	_ = store.Put(ctx, note.Note{Key: "harden:widget", Run: run, Name: "fix_plan", Author: "claude"}, strings.NewReader("plan A"))
	_ = store.Put(ctx, note.Note{Key: "harden:widget", Run: run, Name: "fix_plan", Author: "gemini"}, strings.NewReader("plan B"))

	_, rc, err := store.Get(ctx, "harden:widget", run, "fix_plan", "claude")
	if err != nil {
		panic(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		panic(err)
	}
	fmt.Printf("claude fix_plan: %s\n", body)

	// Gather every author's fix_plan for this key+run — the adversarial-review fan-in.
	page, err := store.List(ctx, note.Filter{Key: "harden:widget", Run: &run, Name: "fix_plan"})
	if err != nil {
		panic(err)
	}
	fmt.Println("fix_plans in run:", len(page.Notes))

	// Output:
	// claude fix_plan: plan A
	// fix_plans in run: 2
}
