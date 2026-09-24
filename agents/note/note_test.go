/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package note_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/note"
	"chainguard.dev/driftlessaf/agents/note/notetest"
)

// TestMem_Conformance drives the in-memory backend through the shared contract
// suite every backend runs, so Mem cannot drift from the durable backends it
// stands in for in other packages' tests.
func TestMem_Conformance(t *testing.T) {
	notetest.RunConformance(t, func() note.Store { return note.NewMem() })
}

// TestRef pins that Ref is deterministic, that every coordinate is load-bearing,
// and that its length-prefixed framing is injective — no coordinate value can be
// shifted across a field boundary to forge another note's Ref.
func TestRef(t *testing.T) {
	base := note.Ref("harden:widget", 1, "fix_plan", "claude")

	// Deterministic: same coordinates, same Ref.
	if again := note.Ref("harden:widget", 1, "fix_plan", "claude"); again != base {
		t.Errorf("Ref not deterministic: got %q and %q", base, again)
	}

	// Every coordinate changes the Ref.
	for _, tc := range []struct {
		name string
		ref  string
	}{
		{"key", note.Ref("harden:gadget", 1, "fix_plan", "claude")},
		{"run", note.Ref("harden:widget", 2, "fix_plan", "claude")},
		{"name", note.Ref("harden:widget", 1, "critique", "claude")},
		{"author", note.Ref("harden:widget", 1, "fix_plan", "gemini")},
	} {
		if tc.ref == base {
			t.Errorf("Ref ignores the %s coordinate", tc.name)
		}
	}

	// Adjacent string coordinates must not collide: (name, author) ("a","bc") vs
	// ("ab","c") — bare concatenation would make both "abc".
	if note.Ref("k", 1, "a", "bc") == note.Ref("k", 1, "ab", "c") {
		t.Error("Ref collides on adjacent coordinates")
	}
	// Embedded separator bytes must not let a coordinate cross a boundary: a NUL
	// separator would frame ("a","\x00b") and ("a\x00","b") identically.
	// Length-prefixing keeps them distinct regardless of content.
	if note.Ref("k", 1, "a", "\x00b") == note.Ref("k", 1, "a\x00", "b") {
		t.Error("Ref collides on an embedded NUL — framing is not injective")
	}
}

// TestNote_Validate pins the single storable-note gate: Key and Name required,
// Run >= 1, Author and body optional.
func TestNote_Validate(t *testing.T) {
	tests := []struct {
		name    string
		note    note.Note
		wantErr bool
	}{
		{"valid", note.Note{Key: "k", Run: 1, Name: "fix_plan", Author: "claude"}, false},
		{"valid without author (shared scope)", note.Note{Key: "k", Run: 1, Name: "fix_plan"}, false},
		{"zero value", note.Note{}, true},
		{"missing key", note.Note{Run: 1, Name: "n"}, true},
		{"missing name", note.Note{Key: "k", Run: 1}, true},
		{"run zero", note.Note{Key: "k", Run: 0, Name: "n"}, true},
		{"run negative", note.Note{Key: "k", Run: -1, Name: "n"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.note.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate(%+v): got err=%v, wantErr=%t", tt.note, err, tt.wantErr)
			}
		})
	}
}

// TestFilter_Matches pins the selection rule every backend shares, including the
// two cases a backend is most likely to get wrong on its own: a nil Author is a
// wildcard that includes the shared scope, while &"" selects that scope alone.
func TestFilter_Matches(t *testing.T) {
	authored := note.Note{Key: "K", Run: 1, Name: "fix_plan", Author: "claude"}
	shared := note.Note{Key: "K", Run: 1, Name: "fix_plan"}

	run1, run2 := 1, 2
	claude, emptyAuthor := "claude", ""
	tests := []struct {
		name         string
		filter       note.Filter
		wantAuthored bool
		wantShared   bool
	}{
		{"empty filter is all wildcards", note.Filter{}, true, true},
		{"key matches both", note.Filter{Key: "K"}, true, true},
		{"other key matches neither", note.Filter{Key: "other"}, false, false},
		{"run matches both", note.Filter{Run: &run1}, true, true},
		{"other run matches neither", note.Filter{Run: &run2}, false, false},
		{"name matches both", note.Filter{Name: "fix_plan"}, true, true},
		{"other name matches neither", note.Filter{Name: "critique"}, false, false},
		{"nil author is a wildcard over both scopes", note.Filter{Key: "K", Name: "fix_plan"}, true, true},
		{"author selects the authored note", note.Filter{Author: &claude}, true, false},
		{`&"" selects the shared scope alone`, note.Filter{Author: &emptyAuthor}, false, true},
		{"limit and cursor do not select", note.Filter{Limit: 1, Cursor: "abc"}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.filter.Matches(authored); got != tt.wantAuthored {
				t.Errorf("Matches(authored): got %t, want %t", got, tt.wantAuthored)
			}
			if got := tt.filter.Matches(shared); got != tt.wantShared {
				t.Errorf("Matches(shared): got %t, want %t", got, tt.wantShared)
			}
		})
	}
}
