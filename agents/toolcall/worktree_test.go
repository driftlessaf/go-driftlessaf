/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"os"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

// TestWorktreeToolsNegativeOffset is a regression test: the model controls the
// offset parameter of the paginated worktree tools, and callbacks index slices
// with it. The handler must reject a negative offset with an error the model can
// retry, without reaching the callback at all.
func TestWorktreeToolsNegativeOffset(t *testing.T) {
	// Callbacks that fail the test if a rejected call reaches them.
	cb := callbacks.WorktreeCallbacks{
		ReadFile: func(context.Context, string, int64, int) (callbacks.ReadResult, error) {
			t.Error("ReadFile callback invoked with a negative offset")
			return callbacks.ReadResult{}, nil
		},
		ListDirectory: func(context.Context, string, string, int, int) (callbacks.ListResult, error) {
			t.Error("ListDirectory callback invoked with a negative offset")
			return callbacks.ListResult{}, nil
		},
		SearchCodebase: func(context.Context, string, string, string, int, int) (callbacks.SearchResult, error) {
			t.Error("SearchCodebase callback invoked with a negative offset")
			return callbacks.SearchResult{}, nil
		},
	}
	tools := worktreeToolDefs[string](cb)

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"read_file", map[string]any{"path": "foo.go", "offset": float64(-1)}},
		{"list_directory", map[string]any{"path": ".", "offset": float64(-1)}},
		{"search_codebase", map[string]any{"pattern": "func Foo", "offset": float64(-1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, ok := tools[tc.name]
			if !ok {
				t.Fatalf("tool %q not registered", tc.name)
			}
			trace, _ := agenttrace.StartTrace[string](t.Context(), "test")
			resp := tool.Handler(t.Context(), ToolCall{ID: tc.name + "-1", Name: tc.name, Args: tc.args}, trace, nil)
			errMsg, ok := resp["error"].(string)
			if !ok {
				t.Fatalf("response: got = %v, want an error field", resp)
			}
			if !strings.Contains(errMsg, "offset must be >= 0") {
				t.Errorf("error: got = %q, want it to mention the offset bound", errMsg)
			}
		})
	}
}

// TestWorktreeToolsOffsetSchemaMinimum checks the advertised schema tells the
// model that offset starts at zero, so a conforming provider rejects a negative
// value before the call is dispatched.
func TestWorktreeToolsOffsetSchemaMinimum(t *testing.T) {
	cb := callbacks.WorktreeCallbacks{
		ReadFile: func(context.Context, string, int64, int) (callbacks.ReadResult, error) {
			return callbacks.ReadResult{}, nil
		},
		ListDirectory: func(context.Context, string, string, int, int) (callbacks.ListResult, error) {
			return callbacks.ListResult{}, nil
		},
		SearchCodebase: func(context.Context, string, string, string, int, int) (callbacks.SearchResult, error) {
			return callbacks.SearchResult{}, nil
		},
		WriteFile: func(context.Context, string, string, os.FileMode) error { return nil },
	}
	tools := worktreeToolDefs[string](cb)

	for _, name := range []string{"read_file", "list_directory", "search_codebase"} {
		t.Run(name, func(t *testing.T) {
			for _, p := range tools[name].Def.Parameters {
				if p.Name != "offset" {
					continue
				}
				if p.Minimum == nil {
					t.Fatal("offset minimum: got = nil, want = 0")
				}
				if *p.Minimum != 0 {
					t.Errorf("offset minimum: got = %v, want = 0", *p.Minimum)
				}
				return
			}
			t.Error("no offset parameter declared")
		})
	}
}

// TestWorktreeToolsReadFileReportsOffset checks that read_file relays the
// window's actual start, which precedes the requested offset when the window
// was aligned to the beginning of its line.
func TestWorktreeToolsReadFileReportsOffset(t *testing.T) {
	next := int64(40)
	cb := callbacks.WorktreeCallbacks{
		ReadFile: func(_ context.Context, _ string, offset int64, _ int) (callbacks.ReadResult, error) {
			return callbacks.ReadResult{Content: "line\n", Offset: offset - 3, NextOffset: &next, Remaining: 10}, nil
		},
	}
	tool := worktreeToolDefs[string](cb)["read_file"]
	trace, _ := agenttrace.StartTrace[string](t.Context(), "test")
	resp := tool.Handler(t.Context(), ToolCall{ID: "r1", Name: "read_file", Args: map[string]any{"path": "a.go", "offset": float64(10)}}, trace, nil)

	if got, want := resp["offset"], int64(7); got != want {
		t.Errorf("offset: got = %v, want = %v", got, want)
	}
	if got, want := resp["next_offset"], int64(40); got != want {
		t.Errorf("next_offset: got = %v, want = %v", got, want)
	}
	if got, want := resp["remaining"], int64(10); got != want {
		t.Errorf("remaining: got = %v, want = %v", got, want)
	}
}

// TestWorktreeToolsEditFileNote checks that edit_file relays the callback's
// Note only when the edit made an adjustment.
func TestWorktreeToolsEditFileNote(t *testing.T) {
	tests := []struct {
		name     string
		note     string
		wantNote bool
	}{
		{name: "exact match omits note", note: "", wantNote: false},
		{name: "adjusted match includes note", note: "old_string matched after adjusting indentation (added 1 tab); new_string was shifted the same way", wantNote: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cb := callbacks.WorktreeCallbacks{
				EditFile: func(context.Context, string, string, string, bool) (callbacks.EditResult, error) {
					return callbacks.EditResult{Replacements: 1, Note: tc.note}, nil
				},
			}
			tool := worktreeToolDefs[string](cb)["edit_file"]
			trace, _ := agenttrace.StartTrace[string](t.Context(), "test")
			resp := tool.Handler(t.Context(), ToolCall{ID: "e1", Name: "edit_file", Args: map[string]any{"path": "a.go", "old_string": "x", "new_string": "y"}}, trace, nil)

			got, ok := resp["note"]
			if ok != tc.wantNote {
				t.Fatalf("note present: got = %v, want = %v (resp = %v)", ok, tc.wantNote, resp)
			}
			if tc.wantNote && got != tc.note {
				t.Errorf("note: got = %q, want = %q", got, tc.note)
			}
			if got, want := resp["replacements"], 1; got != want {
				t.Errorf("replacements: got = %v, want = %v", got, want)
			}
		})
	}
}
