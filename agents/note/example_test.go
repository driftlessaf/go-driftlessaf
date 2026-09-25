/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package note_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"chainguard.dev/driftlessaf/agents/note"
)

// ExampleNote_Validate checks coordinates before publishing a note.
func ExampleNote_Validate() {
	n := note.Note{Key: "harden:widget", Run: 1, Name: "harden/fix_plan"}
	fmt.Println("valid note:", n.Validate())
	n.Run = 0
	fmt.Println("missing run:", n.Validate())
	// Output:
	// valid note: <nil>
	// missing run: note: Note.Run must be >= 1, got 0
}

// ExampleFilter_Matches selects shared notes from one attempt. A nil Author
// matches every author; a pointer to an empty string selects shared notes only.
func ExampleFilter_Matches() {
	n := note.Note{Key: "harden:widget", Run: 2, Name: "harden/fix_plan", Author: "claude"}
	filter := note.Filter{Key: "harden:widget", Run: new(2), Name: "harden/fix_plan"}
	fmt.Println("any author:", filter.Matches(n))
	filter.Author = new("")
	fmt.Println("authored note:", filter.Matches(n))
	n.Author = ""
	fmt.Println("shared note:", filter.Matches(n))
	n.Run = 1
	fmt.Println("previous run:", filter.Matches(n))
	// Output:
	// any author: true
	// authored note: false
	// shared note: true
	// previous run: false
}

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

// ExampleNewScoped shows the capability handle: a run-scoped handle is what one
// attempt's agents share, and it cannot reach another attempt or another item
// however the caller addresses it. The handle spans every stage of that attempt,
// so a step in a later pipeline stage reads back what an earlier one wrote.
func ExampleNewScoped() {
	ctx := context.Background()
	store := note.NewMem()
	handle, err := note.NewScoped(note.Scope{Key: "harden:widget", Run: 7}, store)
	if err != nil {
		panic(err)
	}

	// An earlier stage of run 7 publishes its plan.
	if err := handle.Put(ctx, note.Note{Key: "harden:widget", Run: 7, Name: "harden/fix_plan", Author: "claude"}, strings.NewReader("plan A")); err != nil {
		panic(err)
	}
	// A previous attempt's note, which this handle must not reach.
	if err := store.Put(ctx, note.Note{Key: "harden:widget", Run: 6, Name: "harden/fix_plan", Author: "claude"}, strings.NewReader("the old plan")); err != nil {
		panic(err)
	}

	// A later stage reads the earlier stage's note on the same handle.
	_, rc, err := handle.Get(ctx, "harden:widget", 7, "harden/fix_plan", "claude")
	if err != nil {
		panic(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		panic(err)
	}
	fmt.Printf("read across stages: %s\n", body)

	// Reaching the previous attempt is refused, not answered.
	_, _, err = handle.Get(ctx, "harden:widget", 6, "harden/fix_plan", "claude")
	fmt.Println("previous run out of scope:", errors.Is(err, note.ErrOutOfScope))

	// An unfiltered List resolves to the handle's own scope rather than the store.
	page, err := handle.List(ctx, note.Filter{})
	if err != nil {
		panic(err)
	}
	fmt.Println("notes in reach:", len(page.Notes))

	// An attempt may retract its own note, but not the previous attempt's.
	err = handle.Delete(ctx, "harden:widget", 6, "harden/fix_plan", "claude")
	fmt.Println("previous run delete out of scope:", errors.Is(err, note.ErrOutOfScope))
	if err := handle.Delete(ctx, "harden:widget", 7, "harden/fix_plan", "claude"); err != nil {
		panic(err)
	}
	_, _, err = handle.Get(ctx, "harden:widget", 7, "harden/fix_plan", "claude")
	fmt.Println("retracted note absent:", errors.Is(err, note.ErrNotExist))

	// Output:
	// read across stages: plan A
	// previous run out of scope: true
	// notes in reach: 1
	// previous run delete out of scope: true
	// retracted note absent: true
}

// ExampleScope_Validate shows that a key is required even for a run-pinned scope.
func ExampleScope_Validate() {
	fmt.Println("key-wide:", (note.Scope{Key: "harden:widget"}).Validate())
	fmt.Println("one attempt:", (note.Scope{Key: "harden:widget", Run: 7}).Validate())
	fmt.Println("run alone:", (note.Scope{Run: 7}).Validate())
	// Output:
	// key-wide: <nil>
	// one attempt: <nil>
	// run alone: note: Scope.Key is required
}

// ExampleScoped_Scope shows that editing the returned scope leaves the handle's
// grant unchanged.
func ExampleScoped_Scope() {
	handle, err := note.NewScoped(note.Scope{Key: "harden:widget", Run: 7}, note.NewMem())
	if err != nil {
		panic(err)
	}

	scope := handle.Scope()
	fmt.Printf("granted scope: %+v\n", scope)
	scope.Run = 0
	fmt.Printf("edited copy: %+v\n", scope)
	fmt.Printf("handle scope: %+v\n", handle.Scope())
	// Output:
	// granted scope: {Key:harden:widget Run:7}
	// edited copy: {Key:harden:widget Run:0}
	// handle scope: {Key:harden:widget Run:7}
}
