/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/go-cmp/cmp"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/driftlessaf/workqueue/conformance"
)

func TestDeadLetterKey(t *testing.T) {
	key := &inProgressKey{
		attrs: &keyAttrs{
			Name: "in-progress/test-key",
		},
	}
	got := key.deadLetterKey()
	want := "dead-letter/test-key"
	if got != want {
		t.Errorf("deadLetterKey(): got = %q, want = %q", got, want)
	}
}

func TestEnumerateAttrSelectionMatchesKeyAttrs(t *testing.T) {
	typ := reflect.TypeFor[keyAttrs]()
	want := make([]string, 0, typ.NumField())
	for field := range typ.Fields() {
		// Only exported, non-embedded fields name a GCS object attribute.
		// SetAttrSelection rejects an unknown attribute name and fails every
		// Enumerate, so anything else on keyAttrs stays out of the selection.
		if !field.IsExported() || field.Anonymous {
			// An embedded *storage.ObjectAttrs reaches an unselected attribute
			// through a promoted field while the selection still looks complete.
			if field.Anonymous && field.Type == reflect.TypeFor[*storage.ObjectAttrs]() {
				t.Errorf("keyAttrs embeds *storage.ObjectAttrs, which reintroduces "+
					"unselected attributes as promoted fields: %v", field.Name)
			}
			continue
		}
		want = append(want, field.Name)
	}
	slices.Sort(want)

	got := slices.Clone(enumerateAttrSelection)
	slices.Sort(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("enumerateAttrSelection does not match keyAttrs (-want +got):\n%s", diff)
	}
}

func TestWorkQueue(t *testing.T) {
	bucket, ok := os.LookupEnv("WORKQUEUE_GCS_TEST_BUCKET")
	if !ok {
		t.Skip("WORKQUEUE_GCS_TEST_BUCKET not set")
	}
	// Adjust this to a suitable period for testing things.
	// The conformance tests own adjusting MaximumBackoffPeriod.
	workqueue.BackoffPeriod = 10 * time.Second

	client, err := storage.NewClient(t.Context())
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	conformance.TestSemantics(t, func(u int) workqueue.Interface {
		return NewWorkQueue(client.Bucket(bucket), u)
	})

	conformance.TestConcurrency(t, func(u int) workqueue.Interface {
		return NewWorkQueue(client.Bucket(bucket), u)
	})

	conformance.TestOwner(t, func(identity string) workqueue.Interface {
		return NewWorkQueue(client.Bucket(bucket), 1, WithIdentity(identity))
	})

	conformance.TestDurability(t, func(u int) workqueue.Interface {
		return NewWorkQueue(client.Bucket(bucket), u)
	})

	conformance.TestMaxRetry(t, func(u int) workqueue.Interface {
		return NewWorkQueue(client.Bucket(bucket), u)
	})

	conformance.TestBackoffDelay(t, func(u int) workqueue.Interface {
		return NewWorkQueue(client.Bucket(bucket), u)
	})
}

// listedFields returns the object attributes named inside the items(...) group
// of a listing's fields mask, and whether the mask also requests the pagination
// token.
func listedFields(t *testing.T, mask string) ([]string, bool) {
	t.Helper()
	start := strings.Index(mask, "items(")
	end := strings.LastIndex(mask, ")")
	if start < 0 || end < start {
		t.Fatalf("fields mask %q has no items(...) group", mask)
	}
	fields := strings.Split(mask[start+len("items("):end], ",")
	slices.Sort(fields)
	return fields, strings.Contains(mask[:start], "nextPageToken")
}

func TestEnumerateListsOnlyNeededFields(t *testing.T) {
	f := &fakeGCS{
		handler: func(gcsCall) (int, string) {
			return http.StatusOK, `{"kind":"storage#objects","items":[]}`
		},
	}
	wq := NewWorkQueue(newTestClient(t, f), 10)

	if _, _, _, err := wq.Enumerate(t.Context()); err != nil {
		t.Fatalf("Enumerate() = %v", err)
	}

	call, ok := findCall(f.recorded(), http.MethodGet, "/b/test-bucket/o")
	if !ok {
		t.Fatalf("no object listing recorded, got calls = %v", f.recorded())
	}

	got, hasToken := listedFields(t, call.query.Get("fields"))
	// Without the pagination token a restricted listing would silently observe
	// only the first page, hiding queued work and orphaned leases.
	if !hasToken {
		t.Errorf("fields = %q, want it to request nextPageToken", call.query.Get("fields"))
	}

	// Exactly the attributes this package reads off a listed object. Anything
	// extra is transferred on every enumeration and never used.
	want := []string{"generation", "metadata", "metageneration", "name", "timeCreated"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("listed fields (-want +got):\n%s", diff)
	}
}

// The narrowed selection must still deliver Generation and Metageneration. They
// pin the preconditions on the keys Enumerate returns, and a zero makes
// RequeueWithOptions read a lease it cannot match and skip orphan recovery.
//
// The selection travels the real path: SetAttrSelection to fields mask to a
// response the fake projects the way GCS does. The values are distinctive so a
// plausible default cannot satisfy the assertions.
func TestEnumerateNarrowedListingCarriesPreconditionFields(t *testing.T) {
	const (
		wantGeneration     = int64(4271)
		wantMetageneration = int64(9)
	)
	f := &fakeGCS{
		handler: func(gcsCall) (int, string) {
			return http.StatusOK, fmt.Sprintf(
				`{"kind":"storage#objects","items":[`+
					`{"name":"queued/a","generation":"%d","metageneration":"%d",`+
					`"timeCreated":"2026-01-01T00:00:00Z"}]}`,
				wantGeneration, wantMetageneration)
		},
	}
	wq := NewWorkQueue(newTestClient(t, f), 10)

	_, qd, _, err := wq.Enumerate(t.Context())
	if err != nil {
		t.Fatalf("Enumerate() = %v", err)
	}
	if len(qd) != 1 {
		t.Fatalf("Enumerate() queued keys = %d, want 1", len(qd))
	}

	qk, ok := qd[0].(*queuedKey)
	if !ok {
		t.Fatalf("queued key type: got = %T, want = *queuedKey", qd[0])
	}
	if got := qk.attrs.Generation; got != wantGeneration {
		t.Errorf("Generation: got = %d, want = %d", got, wantGeneration)
	}
	if got := qk.attrs.Metageneration; got != wantMetageneration {
		t.Errorf("Metageneration: got = %d, want = %d", got, wantMetageneration)
	}
}

func TestEnumeratedObservedKeyRequeuePreservesPreconditions(t *testing.T) {
	const (
		key     = "orphan"
		gen     = int64(1234)
		metagen = int64(7)
	)
	expired := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

	f := &fakeGCS{}
	f.handler = func(call gcsCall) (int, string) {
		switch {
		case call.method == http.MethodGet && strings.HasSuffix(call.path, "/b/test-bucket/o"):
			return http.StatusOK, fmt.Sprintf(`{"items":[{"name":%q,"generation":%q,"metageneration":%q,"timeCreated":%q,"metadata":{"lease-expiration":%q}}]}`,
				inProgressPrefix+key, strconv.FormatInt(gen, 10), strconv.FormatInt(metagen, 10), time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), expired)
		case call.method == http.MethodGet && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
			return http.StatusOK, objectJSON(inProgressPrefix+key, gen, metagen)
		case call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/"):
			return http.StatusOK, rewriteJSON(queuedPrefix+key, 4321)
		case call.method == http.MethodDelete && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
			return http.StatusNoContent, ""
		}
		return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
	}
	wq := NewWorkQueue(newTestClient(t, f), 10)

	wip, _, _, err := wq.Enumerate(t.Context())
	if err != nil {
		t.Fatalf("Enumerate() = %v", err)
	}
	if len(wip) != 1 {
		t.Fatalf("Enumerate() in-progress keys = %d, want 1", len(wip))
	}
	if !wip[0].IsOrphaned() {
		t.Fatal("Enumerate() key IsOrphaned() = false, want true")
	}
	if err := wip[0].Requeue(t.Context()); err != nil {
		t.Fatalf("Requeue() = %v", err)
	}

	calls := f.recorded()
	rewrite, ok := findCall(calls, http.MethodPost, "/rewriteTo/")
	if !ok {
		t.Fatal("Requeue() made no rewrite request")
	}
	if got := rewrite.query.Get("sourceGeneration"); got != strconv.FormatInt(gen, 10) {
		t.Errorf("rewrite sourceGeneration: got = %q, want = %d", got, gen)
	}
	del, ok := findCall(calls, http.MethodDelete, "/o/"+inProgressPrefix+key)
	if !ok {
		t.Fatal("Requeue() made no delete request")
	}
	if got := del.query.Get("ifGenerationMatch"); got != strconv.FormatInt(gen, 10) {
		t.Errorf("delete ifGenerationMatch: got = %q, want = %d", got, gen)
	}
	if got := del.query.Get("ifMetagenerationMatch"); got != strconv.FormatInt(metagen, 10) {
		t.Errorf("delete ifMetagenerationMatch: got = %q, want = %d", got, metagen)
	}
}
