/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package textedit

import (
	"errors"
	"strings"
	"testing"
)

// block is a tab-indented fixture. Byte offsets of each line:
//
//	 0: "func f() {\n"   (11 bytes)
//	11: "\tif a {\n"     (8 bytes)
//	19: "\t\tb()\n"      (6 bytes)
//	25: "\n"             (1 byte)
//	26: "\t\tc()\n"      (6 bytes)
//	32: "\t}\n"          (3 bytes)
//	35: "}\n"            (2 bytes)
const block = "func f() {\n\tif a {\n\t\tb()\n\n\t\tc()\n\t}\n}\n"

func TestMatchIgnoringIndentation(t *testing.T) {
	tests := []struct {
		name string
		file string
		old  string
		want Match
	}{{
		name: "file has one more tab on every line",
		file: block,
		old:  "if a {\n\tb()\n\n\tc()\n}",
		want: Match{Start: 11, End: 34, Line: 2, Shift: Shift{Add: true, Whitespace: "\t"}},
	}, {
		name: "file has one fewer tab on every line",
		file: block,
		old:  "\t\tif a {\n\t\t\tb()\n\n\t\t\tc()\n\t\t}",
		want: Match{Start: 11, End: 34, Line: 2, Shift: Shift{Whitespace: "\t"}},
	}, {
		name: "old ending with newline includes the trailing newline",
		file: block,
		old:  "if a {\n\tb()\n\n\tc()\n}\n",
		want: Match{Start: 11, End: 35, Line: 2, Shift: Shift{Add: true, Whitespace: "\t"}},
	}, {
		name: "old ending with newline when file's last line lacks one",
		file: "a\n\tb",
		old:  "b\n",
		want: Match{Start: 2, End: 4, Line: 2, Shift: Shift{Add: true, Whitespace: "\t"}},
	}, {
		name: "zero shift across CRLF file lines",
		file: "a\r\n  b\r\nc\r\n",
		old:  "  b\nc",
		want: Match{Start: 3, End: 9, Line: 2},
	}, {
		name: "zero shift with CRLF and old ending with newline keeps the CRLF",
		file: "a\r\n  b\r\nc\r\n",
		old:  "  b\nc\n",
		want: Match{Start: 3, End: 11, Line: 2},
	}, {
		name: "single line old",
		file: "a\n\tfoo bar\nb\n",
		old:  "foo bar",
		want: Match{Start: 2, End: 10, Line: 2, Shift: Shift{Add: true, Whitespace: "\t"}},
	}, {
		name: "space shift with a whitespace-only blank line in the file",
		file: "x\n    a\n  \n    b\ny\n",
		old:  "a\n\nb",
		want: Match{Start: 2, End: 16, Line: 2, Shift: Shift{Add: true, Whitespace: "    "}},
	}, {
		name: "match at the very start of the file",
		file: "\ta\n\tb\nc\n",
		old:  "a\nb",
		want: Match{Start: 0, End: 5, Line: 1, Shift: Shift{Add: true, Whitespace: "\t"}},
	}, {
		name: "match on the last line without trailing newline",
		file: "a\n\tb",
		old:  "b",
		want: Match{Start: 2, End: 4, Line: 2, Shift: Shift{Add: true, Whitespace: "\t"}},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MatchIgnoringIndentation(strings.NewReader(tc.file), tc.old)
			if err != nil {
				t.Fatalf("MatchIgnoringIndentation: got error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("match: got = %+v, want = %+v", got, tc.want)
			}
			if got.End > int64(len(tc.file)) {
				t.Fatalf("End: got = %d, want <= %d", got.End, len(tc.file))
			}
			region := tc.file[got.Start:got.End]
			if want := ShiftIndentation(tc.old, got.Shift); normalizeLines(region) != normalizeLines(want) {
				t.Errorf("region: got = %q, want = %q after shifting old", region, want)
			}
		})
	}
}

// normalizeLines trims trailing whitespace from every line and drops a final
// newline, so a CRLF region or one with whitespace-only blank lines compares
// equal to the old_string it matched.
func normalizeLines(s string) string {
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		parts[i] = strings.TrimRight(p, " \t\r")
	}
	return strings.TrimSuffix(strings.Join(parts, "\n"), "\n")
}

func TestMatchIgnoringIndentation_Errors(t *testing.T) {
	tests := []struct {
		name string
		file string
		old  string
		want []string
	}{{
		name: "two candidate regions",
		file: "\tx()\n\ty()\n---\n\t\tx()\n\t\ty()\n",
		old:  "x()\ny()",
		want: []string{"at least 2 regions", "byte offsets 0 and 14", "more surrounding context"},
	}, {
		name: "three candidate regions stop counting at two",
		file: "\tx\n\tx\n\tx\n",
		old:  "x",
		want: []string{"at least 2 regions", "byte offsets 0 and 3"},
	}, {
		name: "no match but first line present",
		file: block,
		old:  "if a {\n\tzzz()\n}",
		want: []string{"first non-blank line occurs at line 2 (byte offset 11)", "re-read"},
	}, {
		name: "no match and first line absent",
		file: block,
		old:  "nothing\nhere",
		want: []string{"does not occur anywhere", "may have changed"},
	}, {
		name: "first line present only after a blank leading line",
		file: "top\n\n\tc()\nend\n",
		old:  "\nc()\nnope",
		want: []string{"first non-blank line occurs at line 3 (byte offset 5)"},
	}, {
		name: "shift differs across lines",
		file: "\tx()\n\t\ty()\n",
		old:  "x()\ny()",
		want: []string{"region at line 1 (byte offset 0)", "not the same on every line", "copy the text exactly"},
	}, {
		name: "zero shift line mixed with shifted line",
		file: "x()\n\ty()\n",
		old:  "x()\ny()",
		want: []string{"not the same on every line"},
	}, {
		name: "tabs in file versus spaces in old",
		file: "\tx()\n",
		old:  "  x()",
		want: []string{"not the same on every line"},
	}, {
		name: "file shorter than old",
		file: "a\n",
		old:  "a\nb\nc",
		want: []string{"first non-blank line occurs at line 1 (byte offset 0)"},
	}, {
		name: "empty old",
		file: block,
		old:  "",
		want: []string{"old_string is empty"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MatchIgnoringIndentation(strings.NewReader(tc.file), tc.old)
			if err == nil {
				t.Fatal("MatchIgnoringIndentation: got error = nil, want non-nil")
			}
			if !strings.HasPrefix(err.Error(), notFound) {
				t.Errorf("error prefix: got = %q, want prefix %q", err, notFound)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error: got = %q, want it to contain %q", err, w)
				}
			}
		})
	}
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestMatchIgnoringIndentation_ReadError(t *testing.T) {
	want := errors.New("disk on fire")
	_, err := MatchIgnoringIndentation(failingReader{err: want}, "x")
	if !errors.Is(err, want) {
		t.Errorf("error: got = %v, want = %v", err, want)
	}
}

func TestShiftString(t *testing.T) {
	tests := []struct {
		name  string
		shift Shift
		want  string
	}{
		{name: "zero shift", shift: Shift{}, want: ""},
		{name: "added one tab", shift: Shift{Add: true, Whitespace: "\t"}, want: "added 1 tab"},
		{name: "removed two tabs", shift: Shift{Whitespace: "\t\t"}, want: "removed 2 tabs"},
		{name: "added four spaces", shift: Shift{Add: true, Whitespace: "    "}, want: "added 4 spaces"},
		{name: "removed one space", shift: Shift{Whitespace: " "}, want: "removed 1 space"},
		{name: "mixed whitespace quoted", shift: Shift{Add: true, Whitespace: " \t"}, want: `added " \t"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.shift.String(); got != tc.want {
				t.Errorf("String: got = %q, want = %q", got, tc.want)
			}
		})
	}
}

func TestShiftIndentation(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		shift Shift
		want  string
	}{{
		name:  "zero shift returns input unchanged",
		in:    "\ta\n\tb",
		shift: Shift{},
		want:  "\ta\n\tb",
	}, {
		name:  "add a tab to every non-blank line",
		in:    "a\n\nb\n",
		shift: Shift{Add: true, Whitespace: "\t"},
		want:  "\ta\n\n\tb\n",
	}, {
		name:  "whitespace-only lines stay blank",
		in:    "a\n  \nb",
		shift: Shift{Add: true, Whitespace: "\t"},
		want:  "\ta\n  \n\tb",
	}, {
		name:  "remove a tab where present and leave other lines alone",
		in:    "\ta\nb\n\t\tc\n",
		shift: Shift{Whitespace: "\t"},
		want:  "a\nb\n\tc\n",
	}, {
		name:  "remove spaces",
		in:    "    a\n    b",
		shift: Shift{Whitespace: "    "},
		want:  "a\nb",
	}, {
		name:  "CRLF lines keep their carriage return",
		in:    "a\r\nb\r\n",
		shift: Shift{Add: true, Whitespace: "\t"},
		want:  "\ta\r\n\tb\r\n",
	}, {
		name:  "carriage-return-only line is blank",
		in:    "a\r\n\r\nb",
		shift: Shift{Add: true, Whitespace: "\t"},
		want:  "\ta\r\n\r\n\tb",
	}, {
		name:  "empty input",
		in:    "",
		shift: Shift{Add: true, Whitespace: "\t"},
		want:  "",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShiftIndentation(tc.in, tc.shift); got != tc.want {
				t.Errorf("ShiftIndentation: got = %q, want = %q", got, tc.want)
			}
		})
	}
}

// lines is "ab\ncd\nef\n" plus a final line without a newline.
//
//	0: "ab\n"  3: "cd\n"  6: "ef\n"  9: "gh"
const lines = "ab\ncd\nef\ngh"

func TestLineStart(t *testing.T) {
	tests := []struct {
		name   string
		offset int64
		bound  int
		want   int64
	}{
		{name: "file start", offset: 0, bound: 1024, want: 0},
		{name: "mid first line has no newline before it", offset: 1, bound: 1024, want: 1},
		{name: "pointing at the first newline", offset: 2, bound: 1024, want: 2},
		{name: "exactly after a newline", offset: 3, bound: 1024, want: 3},
		{name: "mid line", offset: 4, bound: 1024, want: 3},
		{name: "mid last line without trailing newline", offset: 10, bound: 1024, want: 9},
		{name: "at end of input", offset: int64(len(lines)), bound: 1024, want: 9},
		{name: "newline within a two byte bound", offset: 4, bound: 2, want: 3},
		{name: "no newline within bound", offset: 4, bound: 1, want: 4},
		{name: "negative offset clamps to zero", offset: -3, bound: 1024, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LineStart(strings.NewReader(lines), tc.offset, tc.bound)
			if err != nil {
				t.Fatalf("LineStart: got error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("LineStart(%d, %d): got = %d, want = %d", tc.offset, tc.bound, got, tc.want)
			}
		})
	}
}

func TestLineEnd(t *testing.T) {
	size := int64(len(lines))
	tests := []struct {
		name  string
		end   int64
		bound int
		want  int64
	}{
		{name: "file start extends to first newline", end: 0, bound: 1024, want: 3},
		{name: "mid line", end: 4, bound: 1024, want: 6},
		{name: "exactly on a newline", end: 2, bound: 1024, want: 3},
		{name: "just after a newline extends through the next line", end: 3, bound: 1024, want: 6},
		{name: "last line without newline is unchanged", end: 10, bound: 1024, want: 10},
		{name: "newline within a one byte bound", end: 2, bound: 1, want: 3},
		{name: "no newline within bound", end: 3, bound: 1, want: 3},
		{name: "end at size", end: size, bound: 1024, want: size},
		{name: "end beyond size", end: size + 5, bound: 1024, want: size},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LineEnd(strings.NewReader(lines), tc.end, size, tc.bound)
			if err != nil {
				t.Fatalf("LineEnd: got error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("LineEnd(%d, %d, %d): got = %d, want = %d", tc.end, size, tc.bound, got, tc.want)
			}
		})
	}
}

func TestLineBoundsWithoutNewlines(t *testing.T) {
	// A file with no newline at all must read exactly as it would without
	// alignment: neither bound moves.
	const flat = "Hello, World!"
	start, err := LineStart(strings.NewReader(flat), 7, 64<<10)
	if err != nil {
		t.Fatalf("LineStart: %v", err)
	}
	if start != 7 {
		t.Errorf("LineStart: got = %d, want = 7", start)
	}
	end, err := LineEnd(strings.NewReader(flat), 12, int64(len(flat)), 64<<10)
	if err != nil {
		t.Fatalf("LineEnd: %v", err)
	}
	if end != 12 {
		t.Errorf("LineEnd: got = %d, want = 12", end)
	}
}
