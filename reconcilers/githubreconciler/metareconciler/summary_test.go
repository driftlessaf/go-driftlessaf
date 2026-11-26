/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"strings"
	"testing"
)

func TestAccumulatedSummary(t *testing.T) {
	long := strings.Repeat("x", 4096)
	sep := summaryNoteSeparator
	tests := []struct {
		name      string
		recorded  string
		current   string
		stacks    bool
		want      string
		wantBytes int // when nonzero, the encoded length instead of the text
		slack     int // how far under wantBytes the encoded length may land
		// wantPrefix, with wantBytes, is what the result must start with; it
		// must also end with the truncation marker.
		wantPrefix string
	}{{
		name:     "rebuild from the default branch starts over",
		recorded: "first", current: "second", stacks: false,
		want: "second",
	}, {
		name:     "nothing recorded keeps the current note",
		recorded: "", current: "second", stacks: true,
		want: "second",
	}, {
		name:     "empty current note adds nothing",
		recorded: "first", current: "", stacks: true,
		want: "first",
	}, {
		name:     "repeated note adds nothing",
		recorded: "first" + sep + "second", current: "second", stacks: true,
		want: "first" + sep + "second",
	}, {
		// A note is a repeat only when it equals a recorded note. One that
		// merely appears inside an earlier note is a new note.
		name:     "a note contained in an earlier note is still appended",
		recorded: "Fixed IsAllowed and the failing test; refused the workflow edit.", current: "Fixed the failing test", stacks: true,
		want: "Fixed IsAllowed and the failing test; refused the workflow edit." + sep + "Fixed the failing test",
	}, {
		name:     "fix-up appends its note",
		recorded: "first", current: "second", stacks: true,
		want: "first" + sep + "second",
	}, {
		name:     "a note's own blank-line runs collapse, so it never holds the separator",
		recorded: "first", current: "one\n\n\n\ntwo\n \n\t\nthree\n\n", stacks: true,
		want: "first" + sep + "one\n\ntwo\n\nthree",
	}, {
		name:     "two notes of the sanitizer's size fit in full",
		recorded: "a" + long[1:], current: "b" + long[1:], stacks: true,
		want: "a" + long[1:] + sep + "b" + long[1:],
	}, {
		name:     "over the bound the oldest middle notes go, first and latest stay",
		recorded: "first" + sep + "a" + long[1:] + sep + "b" + long[1:] + sep + "c" + long[1:], current: "latest", stacks: true,
		want: "first" + sep + "b" + long[1:] + sep + "c" + long[1:] + sep + "latest",
	}, {
		// The sanitizer keeps one blank line between a note's paragraphs, so
		// a first note of two paragraphs must survive the bound whole, not be
		// split so that its second paragraph is dropped as a middle note.
		name:     "a first note of two paragraphs stays whole over the bound",
		recorded: "Fixed IsAllowed.\n\nRefused the workflow edit the report asked for." + sep + "a" + long[1:] + sep + "b" + long[1:] + sep + "c" + long[1:], current: "latest", stacks: true,
		want: "Fixed IsAllowed.\n\nRefused the workflow edit the report asked for." + sep + "b" + long[1:] + sep + "c" + long[1:] + sep + "latest",
	}, {
		name:     "a latest note over its share is cut, the first stays whole",
		recorded: "a" + long[1:], current: "b" + long[1:] + long + long, stacks: true,
		wantBytes:  len(long) + encodedLen(sep) + maxSummaryNoteBytes,
		wantPrefix: "a" + long[1:] + sep + "b",
	}, {
		// A caller that does not sanitize can pass a first-run summary of any
		// size. Nothing is recorded yet, so no later step would bound it.
		name:     "an oversized first-run note is cut to its share",
		recorded: "", current: "b" + strings.Repeat("x", 70<<10), stacks: true,
		wantBytes:  maxSummaryNoteBytes,
		wantPrefix: "b",
	}, {
		name:     "an oversized note on a rebuild is cut to its share",
		recorded: "first", current: "b" + strings.Repeat("x", 70<<10), stacks: false,
		wantBytes:  maxSummaryNoteBytes,
		wantPrefix: "b",
	}, {
		// json.Marshal writes < as the six bytes \u003c, so the bound counts
		// what lands in the metadata block, not the raw length: the latest
		// note is a quarter of its share in raw bytes and over it encoded. A
		// cut falls between whole brackets, so the result can land up to five
		// bytes under the bound.
		name:     "the bound counts angle brackets as the metadata encodes them",
		recorded: "a" + strings.Repeat("<", 1000), current: "b" + strings.Repeat("<", 1024), stacks: true,
		wantBytes: 1 + 6*1000 + encodedLen(sep) + maxSummaryNoteBytes, slack: 5,
		wantPrefix: "a" + strings.Repeat("<", 1000) + sep + "b",
	}, {
		// A recorded first note over its share cannot come from this function,
		// only from a metadata block edited by hand. The latest note, where a
		// fix-up run's refusal lives, must still survive whole.
		name:     "a recorded first note over its share is cut, the latest stays whole",
		recorded: strings.Repeat("<", 4096), current: "Refused the workflow edit.", stacks: true,
		want: boundNote(strings.Repeat("<", 4096)) + sep + "Refused the workflow edit.",
	}, {
		name:     "an oversized recorded summary is bounded when the run adds nothing",
		recorded: strings.Repeat("<", 4096), current: "", stacks: true,
		want: boundNote(strings.Repeat("<", 4096)),
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := accumulatedSummary(tt.recorded, tt.current, tt.stacks)
			if encodedLen(got) > maxAccumulatedSummaryBytes {
				t.Errorf("encodedLen(accumulatedSummary()) = %d, want <= %d", encodedLen(got), maxAccumulatedSummaryBytes)
			}
			if tt.wantBytes != 0 {
				if n := encodedLen(got); n > tt.wantBytes || n < tt.wantBytes-tt.slack {
					t.Errorf("encodedLen(accumulatedSummary()) = %d, want %d (up to %d under)", n, tt.wantBytes, tt.slack)
				}
				if !strings.HasPrefix(got, tt.wantPrefix) || !strings.HasSuffix(got, summaryTruncationMarker) {
					t.Errorf("accumulatedSummary() = %.20q…%q, want the prefix %.20q… kept and the marker at the end", got, got[len(got)-20:], tt.wantPrefix)
				}
				return
			}
			if got != tt.want {
				t.Errorf("accumulatedSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncateOnRune(t *testing.T) {
	// "é" is two bytes; a cut between them would leave an invalid sequence.
	if got, want := truncateOnRune("aé", 2), "a"; got != want {
		t.Errorf("truncateOnRune(%q, 2) = %q, want %q", "aé", got, want)
	}
	if got, want := truncateOnRune("aé", 3), "aé"; got != want {
		t.Errorf("truncateOnRune(%q, 3) = %q, want %q", "aé", got, want)
	}
}
