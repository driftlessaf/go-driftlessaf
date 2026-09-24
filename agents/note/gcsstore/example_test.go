/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstore_test

import (
	"context"
	"fmt"
	"io"
	"strings"

	"chainguard.dev/driftlessaf/agents/note"
	"chainguard.dev/driftlessaf/agents/note/gcsstore"
	"chainguard.dev/driftlessaf/store/blob"
)

// ExampleNew passes notes through the store the way a multi-agent run does: two
// authors each publish their fix_plan for one key and run, a consumer reads one
// back by its coordinates, and a fan-in lists every author's fix_plan for that
// run without reading the bodies it does not need.
//
// The store is wired over the in-memory blob backend here so the example runs
// offline. In production the backend is a bucket, and root is the tenant
// boundary:
//
//	client, err := storage.NewClient(ctx)
//	store, err := gcsstore.New("orgs/example/", gcs.New(client.Bucket(bucketName)))
func ExampleNew() {
	ctx := context.Background()
	store, err := gcsstore.New("notes/", blob.NewMem())
	if err != nil {
		panic(err)
	}

	run := 1
	for author, plan := range map[string]string{"claude": "plan A", "gemini": "plan B"} {
		n := note.Note{Key: "harden:widget", Run: run, Name: "fix_plan", Author: author}
		if err := store.Put(ctx, n, strings.NewReader(plan)); err != nil {
			panic(err)
		}
	}

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

	// Gather every author's fix_plan for this key and run — one prefix scan.
	page, err := store.List(ctx, note.Filter{Key: "harden:widget", Run: &run, Name: "fix_plan"})
	if err != nil {
		panic(err)
	}
	fmt.Println("fix_plans in run:", len(page.Notes))

	// Output:
	// claude fix_plan: plan A
	// fix_plans in run: 2
}

// ExampleWithMaxNoteSize configures a body ceiling. An oversized write fails
// without replacing the existing body.
func ExampleWithMaxNoteSize() {
	ctx := context.Background()
	store, err := gcsstore.New("notes/", blob.NewMem(), gcsstore.WithMaxNoteSize(4))
	if err != nil {
		panic(err)
	}
	n := note.Note{Key: "harden:widget", Run: 1, Name: "fix_plan"}
	if err := store.Put(ctx, n, strings.NewReader("plan")); err != nil {
		panic(err)
	}
	fmt.Println("oversized write:", store.Put(ctx, n, strings.NewReader("a longer plan")))

	_, rc, err := store.Get(ctx, n.Key, n.Run, n.Name, n.Author)
	if err != nil {
		panic(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		panic(err)
	}
	fmt.Printf("stored body: %s\n", body)
	// Output:
	// oversized write: note body exceeds the 4 byte limit
	// stored body: plan
}

// ExampleStore_List_paging shows the paging a caller must do. A page can be
// short — or empty — while the cursor is live, because the prefix scan bounds
// the objects it examines rather than the matches it finds, so only an empty
// cursor ends a listing.
func ExampleStore_List_paging() {
	ctx := context.Background()
	store, err := gcsstore.New("notes/", blob.NewMem())
	if err != nil {
		panic(err)
	}

	for i := range 5 {
		n := note.Note{Key: "harden:widget", Run: 1, Name: fmt.Sprintf("step_%d", i), Author: "claude"}
		if err := store.Put(ctx, n, strings.NewReader("body")); err != nil {
			panic(err)
		}
	}

	filter := note.Filter{Key: "harden:widget", Limit: 2}
	total := 0
	for {
		page, err := store.List(ctx, filter)
		if err != nil {
			panic(err)
		}
		total += len(page.Notes)
		if page.Cursor == "" {
			break
		}
		filter.Cursor = page.Cursor
	}
	fmt.Println("notes listed:", total)
	// Output:
	// notes listed: 5
}
