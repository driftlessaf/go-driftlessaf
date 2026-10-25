/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"chainguard.dev/driftlessaf/workqueue"
)

// tenantListJSON lists two keys in progress for tenant a, one live and one
// orphaned, and four keys queued at the same priority, oldest first.
func tenantListJSON() string {
	created := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	live := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	object := func(name string, offset int, metadata string) string {
		return fmt.Sprintf(`{"kind":"storage#object","bucket":"test-bucket","name":%q,"generation":"1","metageneration":"1","timeCreated":%q,"metadata":{%s}}`,
			name, created.Add(time.Duration(offset)*time.Second).Format(time.RFC3339), metadata)
	}
	priority := fmt.Sprintf("%q:%q", priorityMetadataKey, formatPriority(100))
	items := []string{
		object("in-progress/a/live", 0, fmt.Sprintf("%s,%q:%q", priority, expirationMetadataKey, live)),
		object("in-progress/c/orphan", 0, priority),
		object("queued/a/1", 1, priority),
		object("queued/a/2", 2, priority),
		object("queued/b/1", 3, priority),
		object("queued/c/1", 4, priority),
	}
	return fmt.Sprintf(`{"kind":"storage#objects","items":[%s]}`, strings.Join(items, ","))
}

func ownerOf(key string) string {
	owner, _, _ := strings.Cut(key, "/")
	return owner
}

func TestEnumerateOrdersTenantsFairly(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want []string
		// wantTenants is nil when the keys must not implement TenantKey.
		wantTenants []string
	}{{
		name: "without WithTenant, oldest first",
		want: []string{"a/1", "a/2", "b/1", "c/1"},
	}, {
		// a has a live key in progress, so its oldest queued key is in round
		// 1; c's orphaned key does not count against it.
		name:        "with WithTenant, by round then age",
		opts:        []Option{WithTenant(ownerOf)},
		want:        []string{"b/1", "c/1", "a/1", "a/2"},
		wantTenants: []string{"b", "c", "a", "a"},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGCS{handler: func(gcsCall) (int, string) {
				return http.StatusOK, tenantListJSON()
			}}
			wq := NewWorkQueue(newTestClient(t, f), 10, tc.opts...)

			wip, queued, _, err := wq.Enumerate(t.Context())
			if err != nil {
				t.Fatalf("Enumerate() = %v", err)
			}
			names := make([]string, 0, len(queued))
			var tenants []string
			for _, key := range queued {
				names = append(names, key.Name())
				if tk, ok := key.(workqueue.TenantKey); ok {
					tenants = append(tenants, tk.Tenant())
				}
			}
			if diff := cmp.Diff(tc.want, names); diff != "" {
				t.Errorf("queued keys (-want, +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantTenants, tenants); diff != "" {
				t.Errorf("queued tenants (-want, +got):\n%s", diff)
			}
			for _, key := range wip {
				_, ok := key.(workqueue.TenantKey)
				if want := tc.wantTenants != nil; ok != want {
					t.Errorf("in-progress key %q implements TenantKey: got = %t, want = %t", key.Name(), ok, want)
				}
			}
		})
	}
}

func TestSetQueuedKeysByTenantBoundsCardinality(t *testing.T) {
	labels := prometheus.Labels{
		"service_name":  baseServiceName,
		"revision_name": baseRevisionName,
		"queue_name":    "tenant-metric-test",
	}
	tenantLabels := func(tenant string) prometheus.Labels {
		l := maps.Clone(labels)
		l["tenant"] = tenant
		l["overflow"] = "false"
		return l
	}
	overflowLabels := maps.Clone(labels)
	overflowLabels["tenant"] = ""
	overflowLabels["overflow"] = "true"

	// Tenant t<i> has i+1 keys, so t0 and t1 fall outside the top 10.
	backlog := map[string]int{}
	for i := range topTenants + 2 {
		backlog[fmt.Sprintf("t%d", i)] = i + 1
	}
	setQueuedKeysByTenant(labels, backlog)

	if got := tenantSeries(t, "tenant-metric-test"); got != topTenants+1 {
		t.Errorf("series: got = %d, want = %d", got, topTenants+1)
	}
	if got := testutil.ToFloat64(mQueuedKeysByTenant.With(tenantLabels("t11"))); got != 12 {
		t.Errorf("t11: got = %v, want = 12", got)
	}
	if got := testutil.ToFloat64(mQueuedKeysByTenant.With(overflowLabels)); got != 1+2 {
		t.Errorf("overflow: got = %v, want = 3", got)
	}

	// A tenant named "other" keeps its own series beside the overflow one.
	backlog = map[string]int{"other": 100}
	for i := range topTenants {
		backlog[fmt.Sprintf("o%d", i)] = 1
	}
	setQueuedKeysByTenant(labels, backlog)
	if got := testutil.ToFloat64(mQueuedKeysByTenant.With(tenantLabels("other"))); got != 100 {
		t.Errorf("tenant other: got = %v, want = 100", got)
	}
	if got := testutil.ToFloat64(mQueuedKeysByTenant.With(overflowLabels)); got != 1 {
		t.Errorf("overflow: got = %v, want = 1", got)
	}
	if got := tenantSeries(t, "tenant-metric-test"); got != topTenants+1 {
		t.Errorf("series with tenant other: got = %d, want = %d", got, topTenants+1)
	}

	// A tenant whose backlog drained loses its series.
	setQueuedKeysByTenant(labels, map[string]int{"t11": 1})
	if got := tenantSeries(t, "tenant-metric-test"); got != 1 {
		t.Errorf("series after drain: got = %d, want = 1", got)
	}
}

// tenantSeries counts the workqueue_queued_keys_by_tenant series of one
// queue, ignoring those other tests left behind.
func tenantSeries(t *testing.T, queueName string) int {
	t.Helper()
	ch := make(chan prometheus.Metric, 100)
	mQueuedKeysByTenant.Collect(ch)
	close(ch)
	n := 0
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("Write() = %v", err)
		}
		for _, l := range pb.GetLabel() {
			if l.GetName() == "queue_name" && l.GetValue() == queueName {
				n++
			}
		}
	}
	return n
}
