/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import "testing"

// TestHasRevision pins which comment bodies count as a note for a revision. A
// note that stops matching after someone edits it in the web UI (which stores
// CRLF line endings) would lift the guard it backs, and a body that matches
// without carrying the revision would hold the guard past a new request.
func TestHasRevision(t *testing.T) {
	const marker = "<!--test:no-changes-->"
	tests := []struct {
		name string
		body string
		want bool
	}{{
		name: "as written",
		body: revisionNote(marker, "rev1", "nothing to do"),
		want: true,
	}, {
		name: "edited in the web UI",
		body: marker + "\r\n" + revisionMarker("rev1") + "\r\nnothing to do, really",
		want: true,
	}, {
		name: "revision line last",
		body: marker + "\n" + revisionMarker("rev1"),
		want: true,
	}, {
		name: "another revision",
		body: revisionNote(marker, "rev2", "nothing to do"),
	}, {
		name: "revision that only starts the same",
		body: revisionNote(marker, "rev10", "nothing to do"),
	}, {
		name: "another marker",
		body: revisionNote("<!--other-->", "rev1", "nothing to do"),
	}, {
		name: "revision quoted below the first line",
		body: marker + "\nsee " + revisionMarker("rev1"),
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasRevision(tt.body, marker, "rev1"); got != tt.want {
				t.Errorf("hasRevision(%q): got = %t, want = %t", tt.body, got, tt.want)
			}
		})
	}
}
