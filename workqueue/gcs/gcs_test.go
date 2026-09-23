/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"errors"
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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

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

// TestEnumeratedObservedKeyDeadletterPreservesPreconditions mirrors the
// requeue test for the orphan sweep's other exit: dead-lettering an observed
// key copies from exactly the leased generation and deletes pinned to the
// generation and the observed metageneration.
func TestEnumeratedObservedKeyDeadletterPreservesPreconditions(t *testing.T) {
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
			return http.StatusOK, fmt.Sprintf(`{"items":[{"name":%q,"generation":%q,"metageneration":%q,"timeCreated":%q,"metadata":{"lease-expiration":%q,"attempts":"268"}}]}`,
				inProgressPrefix+key, strconv.FormatInt(gen, 10), strconv.FormatInt(metagen, 10), time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), expired)
		case call.method == http.MethodGet && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
			return http.StatusOK, objectJSON(inProgressPrefix+key, gen, metagen)
		case call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/"):
			return http.StatusOK, rewriteJSON(deadLetterPrefix+key, 4321)
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
	if len(wip) != 1 || !wip[0].IsOrphaned() {
		t.Fatalf("Enumerate() = %d in-progress keys, want one orphan", len(wip))
	}
	observed, ok := wip[0].(*inProgressKey)
	if !ok {
		t.Fatalf("Enumerate() key type = %T, want *inProgressKey", wip[0])
	}
	if got := observed.GetAttempts(); got != 268 {
		t.Fatalf("GetAttempts() = %d, want 268", got)
	}
	if err := observed.Deadletter(t.Context()); err != nil {
		t.Fatalf("Deadletter() = %v", err)
	}

	calls := f.recorded()
	rewrite, ok := findCall(calls, http.MethodPost, "/rewriteTo/")
	if !ok {
		t.Fatal("Deadletter() made no rewrite request")
	}
	if !strings.Contains(rewrite.path, "dead-letter") {
		t.Errorf("rewrite destination path %q is not under the dead-letter prefix", rewrite.path)
	}
	if got := rewrite.query.Get("sourceGeneration"); got != strconv.FormatInt(gen, 10) {
		t.Errorf("rewrite sourceGeneration: got = %q, want = %d", got, gen)
	}
	del, ok := findCall(calls, http.MethodDelete, "/o/"+inProgressPrefix+key)
	if !ok {
		t.Fatal("Deadletter() made no delete request")
	}
	if got := del.query.Get("ifGenerationMatch"); got != strconv.FormatInt(gen, 10) {
		t.Errorf("delete ifGenerationMatch: got = %q, want = %d", got, gen)
	}
	if got := del.query.Get("ifMetagenerationMatch"); got != strconv.FormatInt(metagen, 10) {
		t.Errorf("delete ifMetagenerationMatch: got = %q, want = %d", got, metagen)
	}
}

// TestEnumeratedObservedKeyDeadletterSkipsRefreshedLease: an owner that
// refreshed the lease after the listing (metageneration moved) keeps its key;
// the sweep's dead-letter neither copies nor deletes.
func TestEnumeratedObservedKeyDeadletterSkipsRefreshedLease(t *testing.T) {
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
			return http.StatusOK, fmt.Sprintf(`{"items":[{"name":%q,"generation":%q,"metageneration":%q,"timeCreated":%q,"metadata":{"lease-expiration":%q,"attempts":"9"}}]}`,
				inProgressPrefix+key, strconv.FormatInt(gen, 10), strconv.FormatInt(metagen, 10), time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), expired)
		case call.method == http.MethodGet && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
			return http.StatusOK, objectJSON(inProgressPrefix+key, gen, metagen+1)
		}
		return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
	}
	wq := NewWorkQueue(newTestClient(t, f), 10)

	wip, _, _, err := wq.Enumerate(t.Context())
	if err != nil {
		t.Fatalf("Enumerate() = %v", err)
	}
	if len(wip) != 1 {
		t.Fatalf("Enumerate() = %d in-progress keys, want 1", len(wip))
	}
	if err := wip[0].(*inProgressKey).Deadletter(t.Context()); !errors.Is(err, workqueue.ErrDeadletterSkipped) {
		t.Fatalf("Deadletter() = %v, want ErrDeadletterSkipped", err)
	}
	calls := f.recorded()
	if _, ok := findCall(calls, http.MethodPost, "/rewriteTo/"); ok {
		t.Error("Deadletter() copied a key whose lease was refreshed after observation")
	}
	if _, ok := findCall(calls, http.MethodDelete, "/o/"+inProgressPrefix+key); ok {
		t.Error("Deadletter() deleted a key whose lease was refreshed after observation")
	}
}

func TestEnumerateWithCapacitySkipsBacklogWhenFull(t *testing.T) {
	const (
		active     = "in-progress/active"
		queued     = "queued/queued"
		deadObject = "dead-letter/dead"
	)
	f := &fakeGCS{
		handler: func(call gcsCall) (int, string) {
			switch call.query.Get("prefix") {
			case inProgressPrefix:
				return http.StatusOK, fmt.Sprintf(
					`{"items":[{"name":%q,"generation":"1","metageneration":"1",`+
						`"timeCreated":"2026-01-01T00:00:00Z","metadata":{"lease-expiration":%q}}]}`,
					active, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			case queuedPrefix:
				return http.StatusOK, listPageJSON("", queued)
			case deadLetterPrefix:
				return http.StatusOK, listPageJSON("", deadObject)
			default:
				return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
			}
		},
	}
	wq := NewWorkQueue(newTestClient(t, f), 1)
	bounded, ok := wq.(workqueue.CapacityAware)
	if !ok {
		t.Fatal("GCS workqueue does not implement CapacityAware")
	}

	wip, next, dead, err := bounded.EnumerateWithCapacity(t.Context(), 1)
	if err != nil {
		t.Fatalf("EnumerateWithCapacity() = %v", err)
	}
	if len(wip) != 1 {
		t.Fatalf("in-progress keys = %d, want 1", len(wip))
	}
	if len(next) != 0 || len(dead) != 1 || dead[0].Name() != "dead" {
		t.Fatalf("full queue returned backlog: queued=%d dead=%v", len(next), dead)
	}

	for _, call := range f.recorded() {
		if got := call.query.Get("prefix"); got == "" {
			t.Errorf("full enumeration made an unscoped listing: %v", call.query)
		}
	}
}

func TestEnumerateWithCapacityRefreshesBoundedBacklogMetrics(t *testing.T) {
	const queueName = "capacity-metrics-test"
	full := false
	f := &fakeGCS{
		handler: func(call gcsCall) (int, string) {
			switch call.query.Get("prefix") {
			case inProgressPrefix:
				if !full {
					return http.StatusOK, listPageJSON("")
				}
				return http.StatusOK, fmt.Sprintf(
					`{"items":[{"name":"in-progress/active","generation":"1","metageneration":"1",`+
						`"timeCreated":"2026-01-01T00:00:00Z","metadata":{"lease-expiration":%q}}]}`,
					time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			case queuedPrefix:
				if full {
					return http.StatusOK, listPageJSON("", "queued/a", "queued/b", "queued/c")
				}
				return http.StatusOK, fmt.Sprintf(
					`{"items":[`+
						`{"name":"queued/a","generation":"1","metageneration":"1","timeCreated":"2026-01-01T00:00:00Z"},`+
						`{"name":"queued/b","generation":"1","metageneration":"1","timeCreated":"2026-01-01T00:00:00Z"},`+
						`{"name":"queued/later","generation":"1","metageneration":"1","timeCreated":"2026-01-01T00:00:00Z",`+
						`"metadata":{"not-before":%q}}]}`,
					time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			case deadLetterPrefix:
				if full {
					return http.StatusOK, listPageJSON("", "dead-letter/a", "dead-letter/b")
				}
				return http.StatusOK, listPageJSON("", "dead-letter/a")
			default:
				return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
			}
		},
	}
	wq := NewWorkQueue(newTestClient(t, f), 1, WithName(queueName))
	bounded := wq.(workqueue.CapacityAware)

	if _, _, _, err := bounded.EnumerateWithCapacity(t.Context(), 1); err != nil {
		t.Fatalf("open-slot EnumerateWithCapacity() = %v", err)
	}
	labels := prometheus.Labels{
		"service_name":  baseServiceName,
		"revision_name": baseRevisionName,
		"queue_name":    queueName,
	}
	if got := testutil.ToFloat64(mQueuedKeys.With(labels)); got != 2 {
		t.Fatalf("queued keys after open-slot enumeration = %v, want 2", got)
	}
	if got := testutil.ToFloat64(mDeadLetteredKeys.With(labels)); got != 1 {
		t.Fatalf("dead-lettered keys after open-slot enumeration = %v, want 1", got)
	}

	full = true
	if _, _, _, err := bounded.EnumerateWithCapacity(t.Context(), 1); err != nil {
		t.Fatalf("full EnumerateWithCapacity() = %v", err)
	}
	// The exact queued count is intentionally last-known at capacity; the
	// bounded gauge is refreshed instead and reports at least one queued key.
	if got := testutil.ToFloat64(mQueuedKeys.With(labels)); got != 2 {
		t.Errorf("queued keys at capacity = %v, want last-known 2", got)
	}
	if got := testutil.ToFloat64(mQueuedKeysLowerBound.With(labels)); got != 1 {
		t.Errorf("queued keys lower bound at capacity = %v, want 1", got)
	}
	if got := testutil.ToFloat64(mDeadLetteredKeys.With(labels)); got != 2 {
		t.Errorf("dead-lettered keys at capacity = %v, want refreshed 2", got)
	}

	full = false
	if _, _, _, err := bounded.EnumerateWithCapacity(t.Context(), 1); err != nil {
		t.Fatalf("open-slot re-enumeration = %v", err)
	}
	if got := testutil.ToFloat64(mQueuedKeysLowerBound.With(labels)); got != 3 {
		t.Errorf("queued keys lower bound after capacity opens = %v, want exact 3", got)
	}
}

func TestEnumerateWithCapacityReadsBacklogAfterOrphanReleasesCapacity(t *testing.T) {
	const (
		orphan = "in-progress/orphan"
		queued = "queued/reclaimable"
		dead   = "dead-letter/old"
	)
	f := &fakeGCS{
		handler: func(call gcsCall) (int, string) {
			switch call.query.Get("prefix") {
			case inProgressPrefix:
				return http.StatusOK, fmt.Sprintf(
					`{"items":[{"name":%q,"generation":"1","metageneration":"1",`+
						`"timeCreated":"2026-01-01T00:00:00Z","metadata":{"lease-expiration":%q}}]}`,
					orphan, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
			case queuedPrefix:
				return http.StatusOK, listPageJSON("", queued)
			case deadLetterPrefix:
				return http.StatusOK, listPageJSON("", dead)
			default:
				return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
			}
		},
	}
	wq := NewWorkQueue(newTestClient(t, f), 1)
	bounded := wq.(workqueue.CapacityAware)

	wip, next, deadKeys, err := bounded.EnumerateWithCapacity(t.Context(), 1)
	if err != nil {
		t.Fatalf("EnumerateWithCapacity() = %v", err)
	}
	if len(wip) != 1 || !wip[0].IsOrphaned() {
		t.Fatalf("in-progress keys = %v, want one orphan", wip)
	}
	if len(next) != 1 || next[0].Name() != "reclaimable" {
		t.Fatalf("queued keys = %v, want [reclaimable]", next)
	}
	if len(deadKeys) != 1 || deadKeys[0].Name() != "old" {
		t.Fatalf("dead-lettered keys = %v, want [old]", deadKeys)
	}

	seen := make(map[string]struct{}, 3)
	for _, call := range f.recorded() {
		seen[call.query.Get("prefix")] = struct{}{}
	}
	for _, prefix := range []string{inProgressPrefix, queuedPrefix, deadLetterPrefix} {
		if _, ok := seen[prefix]; !ok {
			t.Errorf("orphan recovery did not list prefix %q", prefix)
		}
	}
}

func TestEnumerateWithCapacityCountsBoundedReadErrors(t *testing.T) {
	const queueName = "capacity-error-metrics-test"
	f := &fakeGCS{
		handler: func(call gcsCall) (int, string) {
			switch call.query.Get("prefix") {
			case inProgressPrefix:
				return http.StatusOK, fmt.Sprintf(
					`{"items":[{"name":"in-progress/active","generation":"1","metageneration":"1",`+
						`"timeCreated":"2026-01-01T00:00:00Z","metadata":{"lease-expiration":%q}}]}`,
					time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			case queuedPrefix, deadLetterPrefix:
				return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
			default:
				return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
			}
		},
	}
	wq := NewWorkQueue(newTestClient(t, f), 1, WithName(queueName))
	bounded := wq.(workqueue.CapacityAware)
	labels := func(operation string) prometheus.Labels {
		return prometheus.Labels{
			"service_name":  baseServiceName,
			"revision_name": baseRevisionName,
			"queue_name":    queueName,
			"operation":     operation,
		}
	}
	deadBefore := testutil.ToFloat64(mCapacityAwareEnumerationErrors.With(labels("dead-letter")))
	depthBefore := testutil.ToFloat64(mCapacityAwareEnumerationErrors.With(labels("queued-depth")))

	if _, _, _, err := bounded.EnumerateWithCapacity(t.Context(), 1); err != nil {
		t.Fatalf("EnumerateWithCapacity() = %v, want bounded reads to be best effort", err)
	}
	if got := testutil.ToFloat64(mCapacityAwareEnumerationErrors.With(labels("dead-letter"))); got != deadBefore+1 {
		t.Errorf("dead-letter bounded read errors = %v, want %v", got, deadBefore+1)
	}
	if got := testutil.ToFloat64(mCapacityAwareEnumerationErrors.With(labels("queued-depth"))); got != depthBefore+1 {
		t.Errorf("queued-depth bounded read errors = %v, want %v", got, depthBefore+1)
	}
}

func TestEnumerateWithCapacityReadsBacklogWhenSlotIsOpen(t *testing.T) {
	f := &fakeGCS{
		handler: func(call gcsCall) (int, string) {
			switch call.query.Get("prefix") {
			case inProgressPrefix:
				return http.StatusOK, listPageJSON("")
			case queuedPrefix:
				return http.StatusOK, fmt.Sprintf(
					`{"items":[{"name":"queued/next","generation":"1","metageneration":"1",` +
						`"timeCreated":"2026-01-01T00:00:00Z","metadata":{"priority":"00000001"}}]}`)
			case deadLetterPrefix:
				return http.StatusOK, listPageJSON("", "dead-letter/old")
			default:
				return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
			}
		},
	}
	wq := NewWorkQueue(newTestClient(t, f), 1)
	bounded := wq.(workqueue.CapacityAware)
	_, next, dead, err := bounded.EnumerateWithCapacity(t.Context(), 1)
	if err != nil {
		t.Fatalf("EnumerateWithCapacity() = %v", err)
	}
	if len(next) != 1 || next[0].Name() != "next" {
		t.Fatalf("queued keys = %v, want [next]", next)
	}
	if len(dead) != 1 || dead[0].Name() != "old" {
		t.Fatalf("dead-lettered keys = %v, want [old]", dead)
	}

	seen := make(map[string]struct{}, 3)
	for _, call := range f.recorded() {
		seen[call.query.Get("prefix")] = struct{}{}
	}
	for _, prefix := range []string{inProgressPrefix, queuedPrefix, deadLetterPrefix} {
		if _, ok := seen[prefix]; !ok {
			t.Errorf("open-slot enumeration did not list prefix %q", prefix)
		}
	}
}

// TestObservedKeyMalformedAttemptsReturnsZero: a key observed via Enumerate
// has no owner context; a malformed attempts value must log and read as 0
// rather than dereference a nil context.
func TestObservedKeyMalformedAttemptsReturnsZero(t *testing.T) {
	observed := newObservedKey(newTestClient(t, &fakeGCS{}), "orphan", 1234, 7)
	observed.attrs.Metadata[attemptsMetadataKey] = "abc"

	if got := observed.GetAttempts(); got != 0 {
		t.Errorf("GetAttempts() = %d, want 0", got)
	}
}

// TestEnumeratedObservedKeyMalformedAttemptsRequeues: the orphan sweep reads
// attempts on every orphan it lists, so a malformed value must still let the
// key be requeued.
func TestEnumeratedObservedKeyMalformedAttemptsRequeues(t *testing.T) {
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
			return http.StatusOK, fmt.Sprintf(`{"items":[{"name":%q,"generation":%q,"metageneration":%q,"timeCreated":%q,"metadata":{"lease-expiration":%q,"attempts":"abc"}}]}`,
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
	if len(wip) != 1 || !wip[0].IsOrphaned() {
		t.Fatalf("Enumerate() = %d in-progress keys, want one orphan", len(wip))
	}
	observed, ok := wip[0].(*inProgressKey)
	if !ok {
		t.Fatalf("Enumerate() key type = %T, want *inProgressKey", wip[0])
	}
	if got := observed.GetAttempts(); got != 0 {
		t.Errorf("GetAttempts() = %d, want 0", got)
	}
	if err := observed.Requeue(t.Context()); err != nil {
		t.Fatalf("Requeue() = %v", err)
	}

	calls := f.recorded()
	rewrite, ok := findCall(calls, http.MethodPost, "/rewriteTo/")
	if !ok {
		t.Fatal("Requeue() made no rewrite request")
	}
	if !strings.Contains(rewrite.path, queuedPrefix) {
		t.Errorf("rewrite destination path %q is not under the queued prefix", rewrite.path)
	}
	if _, ok := findCall(calls, http.MethodDelete, "/o/"+inProgressPrefix+key); !ok {
		t.Error("Requeue() made no delete request")
	}
}
