/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package note_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"chainguard.dev/driftlessaf/agents/note"
	"chainguard.dev/driftlessaf/agents/note/gcsstore"
	"chainguard.dev/driftlessaf/agents/note/notetest"
	"chainguard.dev/driftlessaf/store/blob"
)

// TestScoped_GCSIsolationAndPaging composes the scope guard with the GCS store's
// real encoding and prefix scans. blob.Mem supplies the object storage without
// replacing the note backend's filtering or paging behavior.
func TestScoped_GCSIsolationAndPaging(t *testing.T) {
	backing, err := gcsstore.New("tenant/notes/", blob.NewMem())
	if err != nil {
		t.Fatalf("gcsstore.New: %v", err)
	}
	keys := []string{"harden:w", "harden:widget", "a/b", "a%2Fb", "..", "."}
	names := []string{"harden/fix_plan", "review/critique"}
	for _, key := range keys {
		for _, run := range []int{1, 2} {
			for _, name := range names {
				if err := backing.Put(t.Context(), note.Note{Key: key, Run: run, Name: name}, nil); err != nil {
					t.Fatalf("Put(%q, %d, %q): %v", key, run, name, err)
				}
			}
		}
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				scopeRun int
				wantRuns []int
			}{
				{"run-pinned", 1, []int{1}},
				{"key-wide", 0, []int{1, 2}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					handle, err := note.NewScoped(note.Scope{Key: key, Run: tc.scopeRun}, backing)
					if err != nil {
						t.Fatalf("NewScoped: %v", err)
					}
					want := make([]note.Note, 0, len(tc.wantRuns)*len(names))
					for _, run := range tc.wantRuns {
						for _, name := range names {
							want = append(want, note.Note{Key: key, Run: run, Name: name})
						}
					}

					var got []note.Note
					filter := note.Filter{Limit: 1}
					pages := 0
					// Bound the walk by the entire fixture, including notes
					// outside the scope, so a broken cursor fails the test.
					for range len(keys)*2*len(names) + 1 {
						page, err := handle.List(t.Context(), filter)
						if err != nil {
							t.Fatalf("List: %v", err)
						}
						if len(page.Notes) > filter.Limit {
							t.Fatalf("page size: got %d, want <= %d", len(page.Notes), filter.Limit)
						}
						got = append(got, page.Notes...)
						pages++
						filter.Cursor = page.Cursor
						if filter.Cursor == "" {
							break
						}
					}
					if filter.Cursor != "" {
						t.Fatalf("final cursor: got %q, want empty", filter.Cursor)
					}
					if pages < 2 {
						t.Errorf("pages: got %d, want >= 2", pages)
					}
					if diff := cmp.Diff(notetest.SortNotes(want), notetest.SortNotes(got)); diff != "" {
						t.Errorf("paged notes (-want, +got):\n%s", diff)
					}
				})
			}
		})
	}
}
