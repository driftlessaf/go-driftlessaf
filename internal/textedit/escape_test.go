/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package textedit

import (
	"strings"
	"testing"
)

func TestUnescapeJSON(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{{
		name:   "tab, newline and quote escapes",
		in:     `\tif s == \"\" {\n\t\treturn\n\t}`,
		want:   "\tif s == \"\" {\n\t\treturn\n\t}",
		wantOK: true,
	}, {
		name:   "escaped backslash decodes to one backslash",
		in:     `STRING_AGG(x, '\\n' ORDER BY y)`,
		want:   `STRING_AGG(x, '\n' ORDER BY y)`,
		wantOK: true,
	}, {
		name:   "HTML escapes from encoding/json",
		in:     `a \u0026\u0026 b \u003c c \u003e d`,
		want:   "a && b < c > d",
		wantOK: true,
	}, {
		name:   "surrogate pair",
		in:     `\ud83d\ude00`,
		want:   "\U0001F600",
		wantOK: true,
	}, {
		name:   "text without escapes decodes to itself",
		in:     "plain text: a/b",
		want:   "plain text: a/b",
		wantOK: true,
	}, {
		name: "raw newline",
		in:   "a\n" + `\t` + "b",
	}, {
		name: "raw tab",
		in:   "\tb",
	}, {
		name: "unescaped double quote",
		in:   `x := "a\n"`,
	}, {
		name: "backslash that starts no JSON escape",
		in:   `^\d+$`,
	}, {
		name: "trailing lone backslash",
		in:   `a\`,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := UnescapeJSON(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok: got = %v, want = %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("decoded: got = %q, want = %q", got, tt.want)
			}
		})
	}
}

func TestUnescapeJSONSequences(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{{
		name: "raw newline beside escaped quotes",
		in:   "vars:\n  java-version: " + `\"21\"`,
		want: "vars:\n  java-version: \"21\"",
	}, {
		name: "raw newline beside an escaped backslash",
		in:   `STRING_AGG(x, '\\n' ORDER BY y)` + "\n  FROM rows",
		want: `STRING_AGG(x, '\n' ORDER BY y)` + "\n  FROM rows",
	}, {
		name: "escaped backslash before an escaped quote",
		in:   `\\\"`,
		want: `\"`,
	}, {
		name: "invalid escapes stay as they are",
		in:   `^\d+\.\d+$ ` + `\t`,
		want: `^\d+\.\d+$ ` + "\t",
	}, {
		name: "surrogate pair",
		in:   `x \ud83d\ude00 y`,
		want: "x \U0001F600 y",
	}, {
		name: "text without escapes is unchanged",
		in:   "a\n\tb \"c\"",
		want: "a\n\tb \"c\"",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UnescapeJSONSequences(tt.in); got != tt.want {
				t.Errorf("decoded: got = %q, want = %q", got, tt.want)
			}
		})
	}
}

func TestEscapedNotFound(t *testing.T) {
	tests := []struct {
		name       string
		offsets    []int64
		want       []string
		wantAbsent string
	}{{
		name:       "one decoded match",
		offsets:    []int64{42},
		want:       []string{"JSON escape sequences", "byte offset 42;"},
		wantAbsent: "unique",
	}, {
		name:    "two decoded matches ask for unique context",
		offsets: []int64{3, 17},
		want:    []string{"JSON escape sequences", "byte offsets 3 and 17", "surrounding context to be unique"},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EscapedNotFound(tt.offsets).Error()
			if !strings.HasPrefix(got, notFound+":") {
				t.Errorf("error: got = %q, want prefix %q", got, notFound+":")
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("error: got = %q, want it to contain %q", got, want)
				}
			}
			if tt.wantAbsent != "" && strings.Contains(got, tt.wantAbsent) {
				t.Errorf("error: got = %q, want it not to contain %q", got, tt.wantAbsent)
			}
		})
	}
}
