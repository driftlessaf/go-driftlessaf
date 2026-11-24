/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-cmp/cmp"
)

// localCallbacks writes files to a temporary directory and returns the
// LocalWorktree callbacks over it, so search offsets and reads come from the
// same backend production uses.
func localCallbacks(t *testing.T, files map[string]string, opts ...callbacks.LocalWorktreeOption) callbacks.WorktreeCallbacks {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	return callbacks.LocalWorktree(root, opts...)
}

// runSearch calls the search_codebase tool built from cb with args.
func runSearch(t *testing.T, cb callbacks.WorktreeCallbacks, args map[string]any) map[string]any {
	t.Helper()
	tool, ok := worktreeToolDefs[string](cb)["search_codebase"]
	if !ok {
		t.Fatal("search_codebase not registered")
	}
	trace, _ := agenttrace.StartTrace[string](t.Context(), "test")
	return tool.Handler(t.Context(), ToolCall{ID: "s1", Name: "search_codebase", Args: args}, trace, nil)
}

// matchWant is the part of a match entry a context test checks.
type matchWant struct {
	Path   string
	Line   int
	Text   string
	Before []string
	After  []string
}

func gotMatches(t *testing.T, resp map[string]any) []matchWant {
	t.Helper()
	entries, ok := resp["matches"].([]map[string]any)
	if !ok {
		t.Fatalf("matches: got = %v, want a list of matches", resp)
	}
	got := make([]matchWant, 0, len(entries))
	for _, e := range entries {
		m := matchWant{Path: e["path"].(string)}
		m.Line, _ = e["line"].(int)
		m.Text, _ = e["text"].(string)
		m.Before, _ = e["before"].([]string)
		m.After, _ = e["after"].([]string)
		got = append(got, m)
	}
	return got
}

func TestSearchCodebaseDefaultResponse(t *testing.T) {
	// The pointer-only response, as encoded for the model.
	const want = `{"count":1,"has_more":false,"matches":[{"length":3,"offset":16,"path":"a.go"}],"path":".","pattern":"Foo"}`

	tests := []struct {
		name     string
		args     map[string]any
		noReader bool
	}{
		{name: "context_lines absent", args: map[string]any{"pattern": "Foo"}},
		{name: "context_lines zero", args: map[string]any{"pattern": "Foo", "context_lines": float64(0)}},
		{name: "context_lines negative", args: map[string]any{"pattern": "Foo", "context_lines": float64(-3)}},
		{name: "no ReadFile callback", args: map[string]any{"pattern": "Foo", "context_lines": float64(3)}, noReader: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cb := localCallbacks(t, map[string]string{"a.go": "package a\n\nfunc Foo() {}\n"})
			reads := 0
			read := cb.ReadFile
			cb.ReadFile = func(ctx context.Context, path string, offset int64, limit int) (callbacks.ReadResult, error) {
				reads++
				return read(ctx, path, offset, limit)
			}
			if tc.noReader {
				cb.ReadFile = nil
			}

			got, err := json.Marshal(runSearch(t, cb, tc.args))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != want {
				t.Errorf("response: got = %s, want = %s", got, want)
			}
			if reads != 0 {
				t.Errorf("ReadFile calls: got = %d, want = 0", reads)
			}
		})
	}
}

func TestSearchCodebaseContextLinesParameter(t *testing.T) {
	read := func(context.Context, string, int64, int) (callbacks.ReadResult, error) {
		return callbacks.ReadResult{}, nil
	}
	search := func(context.Context, string, string, string, int, int) (callbacks.SearchResult, error) {
		return callbacks.SearchResult{}, nil
	}
	tests := []struct {
		name     string
		readFile func(context.Context, string, int64, int) (callbacks.ReadResult, error)
		want     bool
	}{
		{name: "offered with ReadFile", readFile: read, want: true},
		{name: "omitted without ReadFile", readFile: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def := worktreeToolDefs[string](callbacks.WorktreeCallbacks{ReadFile: tc.readFile, SearchCodebase: search})["search_codebase"].Def
			var param *Parameter
			for i := range def.Parameters {
				if def.Parameters[i].Name == "context_lines" {
					param = &def.Parameters[i]
				}
			}
			if got := param != nil; got != tc.want {
				t.Fatalf("context_lines declared: got = %v, want = %v", got, tc.want)
			}
			if got := strings.Contains(def.Description, "context_lines"); got != tc.want {
				t.Errorf("description names context_lines: got = %v, want = %v", got, tc.want)
			}
			if param == nil {
				return
			}
			if param.Maximum == nil {
				t.Fatalf("context_lines maximum: got = nil, want = %d", maxSearchContextLines)
			}
			if *param.Maximum != maxSearchContextLines {
				t.Errorf("context_lines maximum: got = %v, want = %d", *param.Maximum, maxSearchContextLines)
			}
		})
	}
}

func TestSearchCodebaseContextLines(t *testing.T) {
	var twelve strings.Builder
	for i := range 12 {
		fmt.Fprintf(&twelve, "line%02d\n", i+1)
	}
	needleLine := strings.Repeat("a", 1500) + "NEEDLE" + strings.Repeat("b", 494)

	tests := []struct {
		name      string
		files     map[string]string
		pattern   string
		lines     int
		wantLines int
		want      []matchWant
	}{{
		name:      "first line of the file",
		files:     map[string]string{"a.txt": "l1\nl2\nl3\nl4\nl5\n"},
		pattern:   "l1",
		lines:     2,
		wantLines: 2,
		want:      []matchWant{{Path: "a.txt", Line: 1, Text: "l1", After: []string{"l2", "l3"}}},
	}, {
		name:      "last line with a trailing newline",
		files:     map[string]string{"a.txt": "l1\nl2\nl3\nl4\nl5\n"},
		pattern:   "l5",
		lines:     2,
		wantLines: 2,
		want:      []matchWant{{Path: "a.txt", Line: 5, Text: "l5", Before: []string{"l3", "l4"}}},
	}, {
		name:      "last line without a trailing newline",
		files:     map[string]string{"a.txt": "l1\nl2\nl3"},
		pattern:   "l3",
		lines:     1,
		wantLines: 1,
		want:      []matchWant{{Path: "a.txt", Line: 3, Text: "l3", Before: []string{"l2"}}},
	}, {
		name:      "single-line file",
		files:     map[string]string{"a.txt": "only"},
		pattern:   "only",
		lines:     3,
		wantLines: 3,
		want:      []matchWant{{Path: "a.txt", Line: 1, Text: "only"}},
	}, {
		name:      "CRLF line endings",
		files:     map[string]string{"a.txt": "a\r\nb\r\nc\r\n"},
		pattern:   "b",
		lines:     1,
		wantLines: 1,
		want:      []matchWant{{Path: "a.txt", Line: 2, Text: "b", Before: []string{"a"}, After: []string{"c"}}},
	}, {
		name:      "overlapping matches each carry their own context",
		files:     map[string]string{"a.txt": "x1 x2\ny\nx3\n"},
		pattern:   `x\d`,
		lines:     1,
		wantLines: 1,
		want: []matchWant{
			{Path: "a.txt", Line: 1, Text: "x1 x2", After: []string{"y"}},
			{Path: "a.txt", Line: 1, Text: "x1 x2", After: []string{"y"}},
			{Path: "a.txt", Line: 3, Text: "x3", Before: []string{"y"}},
		},
	}, {
		name:      "matches in several files",
		files:     map[string]string{"a.txt": "k\nhit\n", "b.txt": "hit\nz\n"},
		pattern:   "hit",
		lines:     1,
		wantLines: 1,
		want: []matchWant{
			{Path: "a.txt", Line: 2, Text: "hit", Before: []string{"k"}},
			{Path: "b.txt", Line: 1, Text: "hit", After: []string{"z"}},
		},
	}, {
		name:      "a multi-line match is placed on its first line",
		files:     map[string]string{"a.txt": "l1\nl2\nl3\nl4\n"},
		pattern:   `l2\nl3`,
		lines:     1,
		wantLines: 1,
		want:      []matchWant{{Path: "a.txt", Line: 2, Text: "l2", Before: []string{"l1"}, After: []string{"l3"}}},
	}, {
		name:      "context_lines above the cap is clamped",
		files:     map[string]string{"a.txt": twelve.String()},
		pattern:   "line07",
		lines:     9,
		wantLines: maxSearchContextLines,
		want: []matchWant{{
			Path:   "a.txt",
			Line:   7,
			Text:   "line07",
			Before: []string{"line02", "line03", "line04", "line05", "line06"},
			After:  []string{"line08", "line09", "line10", "line11", "line12"},
		}},
	}, {
		name:      "long lines are cut around the match",
		files:     map[string]string{"a.txt": strings.Repeat("p", 600) + "\n" + needleLine + "\n"},
		pattern:   "NEEDLE",
		lines:     1,
		wantLines: 1,
		want: []matchWant{{
			Path:   "a.txt",
			Line:   2,
			Text:   searchContextCut + needleLine[1375:1875] + searchContextCut,
			Before: []string{strings.Repeat("p", maxSearchContextLineBytes) + searchContextCut},
		}},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := runSearch(t, localCallbacks(t, tc.files), map[string]any{"pattern": tc.pattern, "context_lines": float64(tc.lines)})

			if diff := cmp.Diff(tc.want, gotMatches(t, resp)); diff != "" {
				t.Errorf("matches (-want, +got):\n%s", diff)
			}
			if got := resp["context_lines"]; got != tc.wantLines {
				t.Errorf("context_lines: got = %v, want = %d", got, tc.wantLines)
			}
			if note, ok := resp["note"]; ok {
				t.Errorf("note: got = %q, want none", note)
			}
		})
	}
}

func TestClipLine(t *testing.T) {
	long := strings.Repeat("a", 1500) + "NEEDLE" + strings.Repeat("b", 494)
	// "x" shifts every two-byte "é" so that byte 500 falls inside one.
	accented := "x" + strings.Repeat("é", 600)

	tests := []struct {
		name string
		line string
		col  int
		want string
	}{
		{name: "short line is kept", line: "short", col: 0, want: "short"},
		{name: "line at the cap is kept", line: strings.Repeat("a", maxSearchContextLineBytes), col: 0, want: strings.Repeat("a", maxSearchContextLineBytes)},
		{name: "long line is cut at the end", line: long, col: 0, want: long[:500] + searchContextCut},
		{name: "match deep in the line stays in view", line: long, col: 1500, want: searchContextCut + long[1375:1875] + searchContextCut},
		{name: "match near the end keeps the tail", line: long, col: 1990, want: searchContextCut + long[1500:]},
		{name: "cut falls on a character boundary", line: accented, col: 0, want: accented[:499] + searchContextCut},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := clipLine(tc.line, tc.col)
			if got != tc.want {
				t.Errorf("clipLine: got = %q, want = %q", got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("clipLine: got invalid UTF-8 %q", got)
			}
		})
	}
}

func TestFileLinesLineNumbers(t *testing.T) {
	// The rows share f and run in order: offsets out of file order count back
	// from the previous match.
	f := &fileLines{data: "a\nb\nc\nd\n", line: 1}
	// The zero searchPattern matches the empty text that a zero Length names.
	for _, tc := range []struct{ off, want int }{{6, 4}, {2, 2}, {4, 3}, {0, 1}, {7, 4}} {
		if got := f.context(callbacks.Match{Offset: int64(tc.off)}, searchPattern{}, 0).line; got != tc.want {
			t.Errorf("line at offset %d: got = %d, want = %d", tc.off, got, tc.want)
		}
	}
}

func TestSearchCodebaseContextResponseBound(t *testing.T) {
	const lineLen, lineCount = 400, 200
	line := "m" + strings.Repeat("x", lineLen-1) + "\n"
	cb := localCallbacks(t, map[string]string{"a.txt": strings.Repeat(line, lineCount)})

	resp := runSearch(t, cb, map[string]any{"pattern": "m", "limit": float64(lineCount), "context_lines": float64(1)})

	matches := gotMatches(t, resp)
	if len(matches) != lineCount {
		t.Fatalf("matches: got = %d, want = %d", len(matches), lineCount)
	}
	withContext, total := 0, 0
	for i, m := range matches {
		if m.Text == "" {
			continue
		}
		if i != withContext {
			t.Fatalf("match %d carries context after a bare match", i)
		}
		withContext++
		total += len(m.Text)
		for _, l := range append(m.Before, m.After...) {
			total += len(l)
		}
	}
	if total > maxSearchContextBytes {
		t.Errorf("context bytes: got = %d, want <= %d", total, maxSearchContextBytes)
	}
	// Every match past the first bare one sits on a middle line, so its context
	// is three full lines.
	if next := total + 3*(lineLen-1); next <= maxSearchContextBytes {
		t.Errorf("context stopped at %d bytes, but the next match fits in %d", total, maxSearchContextBytes)
	}
	wantNote := fmt.Sprintf("%d of %d matches carry no context", lineCount-withContext, lineCount)
	if note, _ := resp["note"].(string); !strings.HasPrefix(note, wantNote) {
		t.Errorf("note: got = %q, want prefix %q", note, wantNote)
	}
}

func TestSearchCodebaseContextUnavailable(t *testing.T) {
	// Lines of filler that push "hit" past the bytes read per file.
	farFile := strings.Repeat("filler line\n", searchContextFileBytes/len("filler line\n")+1000) + "hit\n"

	tests := []struct {
		name     string
		files    map[string]string
		opts     []callbacks.LocalWorktreeOption
		failRead string
		wantBare []string
		wantNote []string
	}{{
		name:     "unreadable file",
		files:    map[string]string{"a.txt": "hit\n", "b.txt": "hit\n"},
		failRead: "b.txt",
		wantBare: []string{"b.txt"},
		wantNote: []string{"1 of 2 matches carry no context"},
	}, {
		name:     "match past the bytes read per file",
		files:    map[string]string{"a.txt": farFile, "b.txt": "hit\n"},
		wantBare: []string{"a.txt"},
		wantNote: []string{"1 of 2 matches carry no context"},
	}, {
		name:     "search note is kept alone",
		files:    map[string]string{"a.txt": "hit\n", "huge.txt": strings.Repeat("z", 100) + "hit\n"},
		opts:     []callbacks.LocalWorktreeOption{callbacks.WithMaxSearchFileBytes(64), callbacks.WithSkipOversizedSearchFiles()},
		wantNote: []string{"were not searched"},
	}, {
		name:     "search note and context note are joined",
		files:    map[string]string{"a.txt": "hit\n", "b.txt": "hit\n", "huge.txt": strings.Repeat("z", 100) + "hit\n"},
		opts:     []callbacks.LocalWorktreeOption{callbacks.WithMaxSearchFileBytes(64), callbacks.WithSkipOversizedSearchFiles()},
		failRead: "b.txt",
		wantBare: []string{"b.txt"},
		wantNote: []string{"were not searched", " 1 of 2 matches carry no context"},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cb := localCallbacks(t, tc.files, tc.opts...)
			read := cb.ReadFile
			cb.ReadFile = func(ctx context.Context, path string, offset int64, limit int) (callbacks.ReadResult, error) {
				if path == tc.failRead {
					return callbacks.ReadResult{}, errors.New("read refused")
				}
				return read(ctx, path, offset, limit)
			}

			resp := runSearch(t, cb, map[string]any{"pattern": "hit", "context_lines": float64(1)})

			var bare []string
			for _, m := range gotMatches(t, resp) {
				if m.Text == "" {
					bare = append(bare, m.Path)
				}
			}
			if diff := cmp.Diff(tc.wantBare, bare); diff != "" {
				t.Errorf("matches without context (-want, +got):\n%s", diff)
			}
			note, _ := resp["note"].(string)
			for _, want := range tc.wantNote {
				if !strings.Contains(note, want) {
					t.Errorf("note: got = %q, want it to contain %q", note, want)
				}
			}
			if len(tc.wantBare) == 0 && strings.Contains(note, "carry no context") {
				t.Errorf("note: got = %q, want no context note", note)
			}
		})
	}
}

func TestSearchCodebaseContextReadBudget(t *testing.T) {
	const fileCount = 10
	files := make(map[string]string, fileCount)
	for i := range fileCount {
		files[fmt.Sprintf("f%d.txt", i)] = "hit\n"
	}
	cb := localCallbacks(t, files)
	// Every file reads as larger than the per-file window, which LocalWorktree
	// would need megabytes on disk to show. The window keeps the ReadFile
	// contract: it starts at offset 0, fits the limit, and reports more to
	// come.
	var limits []int
	cb.ReadFile = func(_ context.Context, _ string, _ int64, limit int) (callbacks.ReadResult, error) {
		limits = append(limits, limit)
		content := "hit\n" + strings.Repeat("z\n", (limit-4)/2)
		next := int64(len(content))
		return callbacks.ReadResult{Content: content, NextOffset: &next, Remaining: 1}, nil
	}

	resp := runSearch(t, cb, map[string]any{"pattern": "hit", "context_lines": float64(1)})

	reads := maxSearchContextReadBytes / searchContextFileBytes
	if diff := cmp.Diff(slices.Repeat([]int{searchContextFileBytes}, reads), limits); diff != "" {
		t.Errorf("ReadFile limits (-want, +got):\n%s", diff)
	}
	for i, m := range gotMatches(t, resp) {
		if got, want := m.Text != "", i < reads; got != want {
			t.Errorf("match %d in %s carries context: got = %v, want = %v", i, m.Path, got, want)
		}
	}
	wantNote := fmt.Sprintf("%d of %d matches carry no context", fileCount-reads, fileCount)
	if note, _ := resp["note"].(string); !strings.HasPrefix(note, wantNote) {
		t.Errorf("note: got = %q, want prefix %q", note, wantNote)
	}
}

func TestSearchCodebaseContextBackendWindows(t *testing.T) {
	// The first searchContextFileBytes of this file end inside its last line,
	// which holds a match. The line before it is the last whole line read.
	var b strings.Builder
	b.WriteString("hit first\n")
	for b.Len() < searchContextFileBytes-200 {
		b.WriteString("filler line\n")
	}
	lastWhole := strings.Count(b.String(), "\n") + 1
	b.WriteString("hit last whole line\n")
	b.WriteString("hit cut " + strings.Repeat("x", 300) + "\n")
	cut := b.String()

	tests := []struct {
		name     string
		files    map[string]string
		reshape  func(res callbacks.ReadResult, limit int) callbacks.ReadResult
		want     []matchWant
		wantNote string
	}{{
		// Some backends return exactly limit bytes, cut mid-line, and no
		// NextOffset. The partial last line is dropped, so it shows neither as
		// a match nor as the previous match's context.
		name:  "full window without NextOffset keeps whole lines only",
		files: map[string]string{"a.txt": cut},
		reshape: func(r callbacks.ReadResult, limit int) callbacks.ReadResult {
			r.Content = r.Content[:min(len(r.Content), limit)]
			r.NextOffset, r.Remaining = nil, 0
			return r
		},
		want: []matchWant{
			{Path: "a.txt", Line: 1, Text: "hit first", After: []string{"filler line"}},
			{Path: "a.txt", Line: lastWhole, Text: "hit last whole line", Before: []string{"filler line"}},
			{Path: "a.txt"},
		},
		wantNote: "1 of 3 matches carry no context",
	}, {
		// Offsets in a window that does not start at 0 would not line up with
		// the search offsets, so the file gets no context.
		name:  "window not starting at offset 0 is not used",
		files: map[string]string{"a.txt": "hit\nnext\n", "b.txt": "hit\nnext\n"},
		reshape: func(r callbacks.ReadResult, _ int) callbacks.ReadResult {
			r.Offset = 4
			return r
		},
		want:     []matchWant{{Path: "a.txt"}, {Path: "b.txt"}},
		wantNote: "2 of 2 matches carry no context",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cb := localCallbacks(t, tc.files)
			read := cb.ReadFile
			cb.ReadFile = func(ctx context.Context, path string, offset int64, limit int) (callbacks.ReadResult, error) {
				res, err := read(ctx, path, offset, limit)
				return tc.reshape(res, limit), err
			}

			resp := runSearch(t, cb, map[string]any{"pattern": "hit", "context_lines": float64(1)})

			if diff := cmp.Diff(tc.want, gotMatches(t, resp)); diff != "" {
				t.Errorf("matches (-want, +got):\n%s", diff)
			}
			if note, _ := resp["note"].(string); !strings.HasPrefix(note, tc.wantNote) {
				t.Errorf("note: got = %q, want prefix %q", note, tc.wantNote)
			}
		})
	}
}

func TestSearchCodebaseContextChangedBytes(t *testing.T) {
	tests := []struct {
		name    string
		content string
		pattern string
		// clean rewrites what ReadFile returns, as a sanitizing wrapper does.
		clean func(string) string
		// substring makes the backend search for the pattern as plain text.
		substring bool
		want      []matchWant
		wantNote  string
	}{{
		// Stripping each line's \r moves a match on line k back k-1 bytes, so
		// past the first lines its offset lands on another line.
		name:    "carriage returns stripped from a CRLF file",
		content: "hit first\r\n" + strings.Repeat("x\r\n", 10) + "hit\r\n" + strings.Repeat("y\r\n", 10),
		pattern: "hit",
		clean:   func(s string) string { return strings.ReplaceAll(s, "\r", "") },
		want: []matchWant{
			{Path: "a.txt", Line: 1, Text: "hit first", After: []string{"x"}},
			{Path: "a.txt"},
		},
		wantNote: "1 of 2 matches carry no context",
	}, {
		name:     "invisible runes stripped before a match",
		content:  strings.Repeat("\u200b", 4) + "a\nhit\n" + strings.Repeat("z\n", 10),
		pattern:  "h.t",
		clean:    func(s string) string { return strings.ReplaceAll(s, "\u200b", "") },
		want:     []matchWant{{Path: "a.txt"}},
		wantNote: "1 of 1 matches carry no context",
	}, {
		name:    "regexp match",
		content: "x\ncall hit(y)\nz\n",
		pattern: `hit\(\w\)`,
		want:    []matchWant{{Path: "a.txt", Line: 2, Text: "call hit(y)", Before: []string{"x"}, After: []string{"z"}}},
	}, {
		// "hit(" is not a valid regexp, so only its plain text confirms the
		// match.
		name:      "plain text match",
		content:   "x\ncall hit(y)\nz\n",
		pattern:   "hit(",
		substring: true,
		want:      []matchWant{{Path: "a.txt", Line: 2, Text: "call hit(y)", Before: []string{"x"}, After: []string{"z"}}},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cb := localCallbacks(t, map[string]string{"a.txt": tc.content})
			if tc.clean != nil {
				read := cb.ReadFile
				cb.ReadFile = func(ctx context.Context, path string, offset int64, limit int) (callbacks.ReadResult, error) {
					res, err := read(ctx, path, offset, limit)
					res.Content = tc.clean(res.Content)
					return res, err
				}
			}
			if tc.substring {
				cb.SearchCodebase = func(_ context.Context, _, pattern, _ string, _, _ int) (callbacks.SearchResult, error) {
					return callbacks.SearchResult{Matches: []callbacks.Match{{
						Path: "a.txt", Offset: int64(strings.Index(tc.content, pattern)), Length: len(pattern),
					}}}, nil
				}
			}

			resp := runSearch(t, cb, map[string]any{"pattern": tc.pattern, "context_lines": float64(1)})

			if diff := cmp.Diff(tc.want, gotMatches(t, resp)); diff != "" {
				t.Errorf("matches (-want, +got):\n%s", diff)
			}
			note, _ := resp["note"].(string)
			if tc.wantNote == "" && strings.Contains(note, "carry no context") {
				t.Errorf("note: got = %q, want no context note", note)
			}
			if !strings.HasPrefix(note, tc.wantNote) {
				t.Errorf("note: got = %q, want prefix %q", note, tc.wantNote)
			}
		})
	}
}
