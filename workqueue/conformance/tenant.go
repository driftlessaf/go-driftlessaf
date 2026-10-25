/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package conformance

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"chainguard.dev/driftlessaf/workqueue"
)

// TestTenantFairness checks the ordering of a queue constructed with a
// TenantFunc. ctor receives the enumeration limit and the TenantFunc, which
// assigns each key to the segment of its name before the first "/".
//
// The scenario is a single tenant flooding one priority: tenant "a" holds
// 1,000 keys at priority 100 when tenant "b" queues one newer key at the same
// priority. Ordered by creation time alone, b's key would sit behind all of
// a's and never be enumerated.
func TestTenantFairness(t *testing.T, ctor func(limit int, tenant workqueue.TenantFunc) workqueue.Interface) {
	const limit = 10
	wq := ctor(limit, func(key string) string {
		tenant, _, _ := strings.Cut(key, "/")
		return tenant
	})
	if wq == nil {
		t.Fatal("NewWorkQueue returned nil")
	}
	if err := drain(t, wq); err != nil {
		t.Fatalf("Drain failed: %v", err)
	}
	t.Cleanup(func() {
		if err := drain(t, wq); err != nil {
			t.Fatalf("Drain failed: %v", err)
		}
	})
	ctx := t.Context()

	flood := make([]string, 1000)
	for i := range flood {
		flood[i] = fmt.Sprintf("a/%04d", i)
		if err := wq.Queue(ctx, flood[i], workqueue.Options{Priority: 100}); err != nil {
			t.Fatalf("Queue(%q) failed: %v", flood[i], err)
		}
	}
	if err := wq.Queue(ctx, "b/0", workqueue.Options{Priority: 100}); err != nil {
		t.Fatalf("Queue(b/0) failed: %v", err)
	}

	// Each tenant's oldest key is in round 0, so b's key follows a's oldest.
	want := append([]string{flood[0], "b/0"}, flood[1:limit-1]...)
	_, queued := checkQueue(t, wq, ExpectedState{Queued: want})
	checkTenants(t, queued)

	// With one of a's keys in flight, a's oldest queued key is in round 1, so
	// b's key goes first.
	owned, err := queued[0].Start(ctx)
	if err != nil {
		t.Fatalf("Start(%q) failed: %v", queued[0].Name(), err)
	}
	want = append([]string{"b/0"}, flood[1:limit]...)
	inProgress, queued := checkQueue(t, wq, ExpectedState{
		WorkInProgress: []string{flood[0]},
		Queued:         want,
	})
	checkTenants(t, queued)
	tk, ok := inProgress[0].(workqueue.TenantKey)
	if !ok {
		t.Fatalf("in-progress key %q does not implement workqueue.TenantKey", inProgress[0].Name())
	}
	if got := tk.Tenant(); got != "a" {
		t.Errorf("Tenant() of %q: got = %q, want = %q", inProgress[0].Name(), got, "a")
	}
	if err := owned.Complete(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
}

// checkTenants checks that every queued key reports the tenant its name
// carries.
func checkTenants(t *testing.T, queued []workqueue.QueuedKey) {
	t.Helper()
	want := make([]string, 0, len(queued))
	got := make([]string, 0, len(queued))
	for _, key := range queued {
		tenant, _, _ := strings.Cut(key.Name(), "/")
		want = append(want, tenant)
		tk, ok := key.(workqueue.TenantKey)
		if !ok {
			t.Fatalf("queued key %q does not implement workqueue.TenantKey", key.Name())
		}
		got = append(got, tk.Tenant())
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("tenants of the queued keys (-want, +got):\n%s", diff)
	}
}
