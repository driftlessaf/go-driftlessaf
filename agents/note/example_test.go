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

// ExampleFilter_Matches selects shared fix plans for one key across all runs.
func ExampleFilter_Matches() {
	filter := note.Filter{Key: "harden:widget", Name: "fix_plan", Author: new("")}
	fmt.Println("shared:", filter.Matches(note.Note{Key: "harden:widget", Run: 1, Name: "fix_plan"}))
	fmt.Println("named author:", filter.Matches(note.Note{Key: "harden:widget", Run: 1, Name: "fix_plan", Author: "claude"}))
	fmt.Println("another run:", filter.Matches(note.Note{Key: "harden:widget", Run: 2, Name: "fix_plan"}))
	// Output:
	// shared: true
	// named author: false
	// another run: true
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

// ExampleStore_Delete shows the retention sweep the store is shaped for. It owns
// no retention policy, so an application that keeps only the newest runs — or
// that must honor a deletion request — walks the notes it wants gone with List
// and removes each with Delete. There is no delete-by-filter on purpose: each
// removal is its own durable decision, so a sweep interrupted half way through
// has still recorded exactly what it did.
func ExampleStore_Delete() {
	ctx := context.Background()
	store := note.NewMem()

	for _, run := range []int{1, 2, 3} {
		n := note.Note{Key: "harden:widget", Run: run, Name: "harden/fix_plan", Author: "claude"}
		if err := store.Put(ctx, n, strings.NewReader("plan")); err != nil {
			panic(err)
		}
	}

	// Retain only the newest run: page the older ones and delete each.
	const retain = 3
	filter := note.Filter{Key: "harden:widget"}
	for {
		page, err := store.List(ctx, filter)
		if err != nil {
			panic(err)
		}
		for _, n := range page.Notes {
			if n.Run >= retain {
				continue
			}
			if err := store.Delete(ctx, n.Key, n.Run, n.Name, n.Author); err != nil {
				panic(err)
			}
		}
		if page.Cursor == "" {
			break
		}
		filter.Cursor = page.Cursor
	}

	remaining, err := store.List(ctx, note.Filter{Key: "harden:widget"})
	if err != nil {
		panic(err)
	}
	fmt.Println("runs retained:", len(remaining.Notes))
	// Output:
	// runs retained: 1
}
