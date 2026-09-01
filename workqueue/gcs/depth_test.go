/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// listPageJSON renders a list response carrying a continuation token, so a test
// can prove the count stops without draining the queue.
func listPageJSON(nextPageToken string, names ...string) string {
	items := make([]string, 0, len(names))
	for _, name := range names {
		items = append(items, fmt.Sprintf(`{"kind":"storage#object","bucket":"test-bucket","name":%q}`, name))
	}
	token := ""
	if nextPageToken != "" {
		token = fmt.Sprintf(`,"nextPageToken":%q`, nextPageToken)
	}
	return fmt.Sprintf(`{"kind":"storage#objects","items":[%s]%s}`, strings.Join(items, ","), token)
}

func TestQueuedDepthCountsTheQueuedKeys(t *testing.T) {
	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		return 200, listPageJSON("", "queued/a", "queued/b", "queued/c")
	}}

	got, err := QueuedDepth(t.Context(), newTestClient(t, f), testQueueName, 10)
	if err != nil {
		t.Fatalf("QueuedDepth() = %v", err)
	}
	if got != 3 {
		t.Errorf("QueuedDepth() = %d, want 3", got)
	}
}

func TestQueuedDepthAsksOnlyForQueuedKeysAndOnlyForTheirNames(t *testing.T) {
	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		return 200, listPageJSON("", "queued/a")
	}}

	if _, err := QueuedDepth(t.Context(), newTestClient(t, f), testQueueName, 10); err != nil {
		t.Fatalf("QueuedDepth() = %v", err)
	}

	call, ok := findCall(f.recorded(), "GET", "/b/test-bucket/o")
	if !ok {
		t.Fatal("no list call was made")
	}
	// In-progress and dead-lettered keys are not backpressure, and listing them
	// would make the answer cost more than it is worth.
	if got := call.query.Get("prefix"); got != queuedPrefix {
		t.Errorf("prefix: got = %q, want %q", got, queuedPrefix)
	}
	// Without a field selection the API returns every object's full metadata,
	// which is most of the bytes and none of the answer.
	fields := call.query.Get("fields")
	if fields == "" {
		t.Error("the list call selects no fields, so it fetches full object metadata")
	}
	if strings.Contains(fields, "metadata") || strings.Contains(fields, "generation") {
		t.Errorf("fields: got = %q, want only the name", fields)
	}
	// Without maxResults the API sends its default page — up to 1,000 names — so
	// the bound would hold over pages rather than over items.
	if got := call.query.Get("maxResults"); got != "10" {
		t.Errorf("maxResults: got = %q, want %q", got, "10")
	}
}

func TestQueuedDepthStopsAtTheLimitWithoutDrainingTheQueue(t *testing.T) {
	// The claim the helper makes is that its cost is bounded by the limit rather
	// than by the backlog. A second page that is never fetched is what proves it.
	pages := 0
	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		pages++
		return 200, listPageJSON("more", "queued/a", "queued/b", "queued/c")
	}}

	got, err := QueuedDepth(t.Context(), newTestClient(t, f), testQueueName, 2)
	if err != nil {
		t.Fatalf("QueuedDepth() = %v", err)
	}
	if got != 2 {
		t.Errorf("QueuedDepth() = %d, want 2", got)
	}
	if pages != 1 {
		t.Errorf("list calls: got = %d, want 1 — the limit must bound the work", pages)
	}
}

func TestQueuedDepthFollowsPagesUntilTheLimitIsReached(t *testing.T) {
	// The companion to the test above: stopping at one page has to be the limit
	// doing its job, not the count being unable to page at all.
	pages := 0
	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		pages++
		if pages == 1 {
			return 200, listPageJSON("more", "queued/a", "queued/b")
		}
		return 200, listPageJSON("", "queued/c")
	}}

	got, err := QueuedDepth(t.Context(), newTestClient(t, f), testQueueName, 10)
	if err != nil {
		t.Fatalf("QueuedDepth() = %v", err)
	}
	if got != 3 {
		t.Errorf("QueuedDepth() = %d, want 3", got)
	}
	if pages != 2 {
		t.Errorf("list calls: got = %d, want 2", pages)
	}
}

func TestQueuedDepthSaturatesAtTheLimit(t *testing.T) {
	// A result equal to the limit is the "at least this many" answer, and it is
	// the SAME answer for a queue of exactly the limit and for a much deeper one.
	// A producer deciding whether to enqueue needs no more than that, and finding
	// out would cost a request past the bound.
	for _, tc := range []struct {
		name  string
		names []string
	}{
		{"exactly the limit", []string{"queued/a", "queued/b"}},
		{"deeper than the limit", []string{"queued/a", "queued/b", "queued/c", "queued/d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGCS{handler: func(gcsCall) (int, string) {
				return 200, listPageJSON("", tc.names...)
			}}
			got, err := QueuedDepth(t.Context(), newTestClient(t, f), testQueueName, 2)
			if err != nil {
				t.Fatalf("QueuedDepth() = %v", err)
			}
			if got != 2 {
				t.Errorf("QueuedDepth() = %d, want 2", got)
			}
		})
	}
}

func TestQueuedDepthOnAnEmptyQueue(t *testing.T) {
	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		return 200, listPageJSON("")
	}}

	got, err := QueuedDepth(t.Context(), newTestClient(t, f), testQueueName, 10)
	if err != nil {
		t.Fatalf("QueuedDepth() = %v", err)
	}
	if got != 0 {
		t.Errorf("QueuedDepth() = %d, want 0", got)
	}
}

const testQueueName = "depth-test"

func TestQueuedDepthRefusesALimitBelowOne(t *testing.T) {
	// No count satisfies the contract, and a threshold misconfigured to zero
	// should be heard about rather than read as "always full" or "always room".
	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		t.Error("QueuedDepth made a request for a limit below 1")
		return 200, listPageJSON("")
	}}

	for _, limit := range []int{0, -1} {
		if _, err := QueuedDepth(t.Context(), newTestClient(t, f), testQueueName, limit); err == nil {
			t.Errorf("QueuedDepth(limit=%d) error = nil, want a refusal", limit)
		}
	}
}

func TestQueuedDepthReturnsTheListError(t *testing.T) {
	// A depth that cannot be read must not read as an empty queue: a producer
	// would take that as room to enqueue.
	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		return 503, errorJSON(503)
	}}

	got, err := QueuedDepth(t.Context(), newTestClient(t, f), testQueueName, 10)
	if err == nil {
		t.Fatal("QueuedDepth() = nil, want the list failure reported")
	}
	if got != 0 {
		t.Errorf("count on error: got = %d, want 0", got)
	}
}

func TestQueuedDepthReturnsZeroWhenAPageAfterTheFirstFails(t *testing.T) {
	// The count reached before the failure is NOT headroom. A caller that logs
	// the error and carries on would read a partial count as room to enqueue,
	// which is the reading the whole function exists to prevent — and it is what
	// Enumerate answers in the same situation.
	pages := 0
	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		pages++
		if pages == 1 {
			return 200, listPageJSON("more", "queued/a", "queued/b", "queued/c")
		}
		return 503, errorJSON(503)
	}}

	got, err := QueuedDepth(t.Context(), newTestClient(t, f), testQueueName, 10)
	if err == nil {
		t.Fatal("QueuedDepth() = nil, want the list failure reported")
	}
	if got != 0 {
		t.Errorf("count on error: got = %d, want 0 — a partial count reads as headroom", got)
	}
}

func TestQueuedDepthCountsItsOwnFailures(t *testing.T) {
	// Polled rather than called once per pass, so a depth read that is failing
	// has to be distinguishable from a queue that is legitimately full.
	const queue = "depth-error-metric-test"
	labels := prometheus.Labels{
		"service_name":  baseServiceName,
		"revision_name": baseRevisionName,
		"queue_name":    queue,
	}
	before := testutil.ToFloat64(mQueuedDepthErrors.With(labels))

	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		return 503, errorJSON(503)
	}}
	if _, err := QueuedDepth(t.Context(), newTestClient(t, f), queue, 10); err == nil {
		t.Fatal("QueuedDepth() = nil, want the list failure reported")
	}

	if got := testutil.ToFloat64(mQueuedDepthErrors.With(labels)); got != before+1 {
		t.Errorf("error counter: got = %v, want %v", got, before+1)
	}
}
