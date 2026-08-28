/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestParsePriority(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{formatPriority(0), 0},
		{formatPriority(100), 100},
		{formatPriority(1000000), 1000000},
		// Unpadded values written by older versions of this package.
		{"100", 100},
		{"0", 0},
		// Absent or malformed reads as the lowest priority.
		{"", 0},
		{"garbage", 0},
	}
	for _, tc := range tests {
		if got := parsePriority(tc.in); got != tc.want {
			t.Errorf("parsePriority(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// queuedObjectJSON renders a queued object carrying only a priority in its
// metadata, so updateMetadata's not-before merge stays inert and priority alone
// decides whether it issues an update.
func queuedObjectJSON(name, priority string) string {
	metadata := ""
	if priority != "" {
		metadata = fmt.Sprintf(`,"metadata":{%q:%q}`, priorityMetadataKey, priority)
	}
	return fmt.Sprintf(`{"kind":"storage#object","bucket":"test-bucket","name":%q,"generation":"1","metageneration":"1"%s}`, name, metadata)
}

// TestUpdateMetadataPriority pins the max-wins priority merge against the
// representations that can be on a queued object. Queue writes priorities
// zero-padded, but older versions of RequeueWithOptions wrote them unpadded, so
// a string compare of the two disagrees with a numeric one in both directions:
// it blocks a legitimate escalation ("50" > "00000100") and lets a requeue lower
// a queued twin ("00000399" < "350").
//
// The escalation direction also has a conformance scenario ("requeue keeps
// priority comparable for dedup"). The lowering direction only lives here,
// because reaching it end to end depends on a rewrite onto an existing
// destination failing its DoesNotExist precondition so that requeueOnce falls
// through to updateMetadata. Real GCS enforces that; fake-gcs-server does not,
// and lets the rewrite clobber the twin instead. Driving updateMetadata directly
// keeps the case deterministic on any backend.
func TestUpdateMetadataPriority(t *testing.T) {
	tests := []struct {
		name string
		// existing and incoming are the raw metadata values, so cases can mix
		// the padded form Queue writes with the unpadded form older versions of
		// RequeueWithOptions wrote. "" for existing means the object carries no
		// priority metadata at all.
		existing string
		incoming string
		// want is the priority the update must carry; "" means updateMetadata
		// must not issue an update at all.
		want string
	}{{
		name:     "legacy unpadded existing still escalates",
		existing: "50",
		incoming: formatPriority(100),
		want:     formatPriority(100),
	}, {
		name:     "legacy unpadded incoming does not lower a padded twin",
		existing: formatPriority(399),
		incoming: "350",
		want:     "",
	}, {
		name:     "legacy unpadded existing is not lowered",
		existing: "350",
		incoming: formatPriority(100),
		want:     "",
	}, {
		name:     "padded existing escalates",
		existing: formatPriority(50),
		incoming: formatPriority(100),
		want:     formatPriority(100),
	}, {
		name:     "equal priority is not rewritten",
		existing: formatPriority(100),
		incoming: formatPriority(100),
		want:     "",
	}, {
		name:     "absent priority is set",
		existing: "",
		incoming: formatPriority(100),
		want:     formatPriority(100),
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGCS{handler: func(call gcsCall) (int, string) {
				return http.StatusOK, queuedObjectJSON(queuedPrefix+"foo", tc.existing)
			}}
			client := newTestClient(t, f)

			if err := updateMetadata(t.Context(), client, "foo", map[string]string{
				priorityMetadataKey: tc.incoming,
			}); err != nil {
				t.Fatalf("updateMetadata(foo, priority=%q) = %v, want nil", tc.incoming, err)
			}

			patch, updated := findCall(f.recorded(), "PATCH", queuedPrefix+"foo")
			if tc.want == "" {
				if updated {
					t.Errorf("updateMetadata(foo, priority=%q) over existing priority %q issued an update, want none", tc.incoming, tc.existing)
				}
				return
			}
			if !updated {
				t.Fatalf("updateMetadata(foo, priority=%q) over existing priority %q issued no update, want priority %q", tc.incoming, tc.existing, tc.want)
			}
			var body struct {
				Metadata map[string]string `json:"metadata"`
			}
			if err := json.Unmarshal([]byte(patch.body), &body); err != nil {
				t.Fatalf("unmarshaling PATCH body %q = %v", patch.body, err)
			}
			if got := body.Metadata[priorityMetadataKey]; got != tc.want {
				t.Errorf("updateMetadata(foo, priority=%q) over existing priority %q set priority %q, want %q", tc.incoming, tc.existing, got, tc.want)
			}
		})
	}
}
