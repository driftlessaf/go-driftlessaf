/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

type testKey struct {
	name     string
	tenant   string
	priority int64
	created  time.Time
}

// legacySelect is the selection the gcs queue made before tenants existed:
// insert, sort by priority, creation time and name, truncate to limit.
func legacySelect(keys []testKey, limit int) []string {
	var qd []testKey
	for _, k := range keys {
		qd = append(qd, k)
		sort.Slice(qd, func(i, j int) bool {
			if lhs, rhs := qd[i].priority, qd[j].priority; lhs != rhs {
				return lhs > rhs
			}
			if !qd[i].created.Equal(qd[j].created) {
				return qd[i].created.Before(qd[j].created)
			}
			return qd[i].name < qd[j].name
		})
		if len(qd) > limit {
			qd = qd[:limit]
		}
	}
	names := make([]string, 0, len(qd))
	for _, k := range qd {
		names = append(names, k.name)
	}
	return names
}

func selectNames(keys []testKey, inflight []string, limit int) []string {
	s := NewTenantSelector[testKey](limit)
	for _, tenant := range inflight {
		s.InProgress(tenant)
	}
	for _, k := range keys {
		s.Add(k, k.tenant, k.name, k.priority, k.created)
	}
	selected := s.Select()
	names := make([]string, 0, len(selected))
	for _, k := range selected {
		names = append(names, k.name)
	}
	return names
}

// TestTenantSelectorSingleTenantMatchesLegacyOrder pins that a queue without a
// TenantFunc, where every key has tenant "", selects exactly the keys, in
// exactly the order, that it selected before tenants existed.
func TestTenantSelectorSingleTenantMatchesLegacyOrder(t *testing.T) {
	base := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := range 500 {
		n := rng.IntN(300)
		limit := rng.IntN(40)
		keys := make([]testKey, n)
		for i := range keys {
			keys[i] = testKey{
				name:     fmt.Sprintf("key-%d-%d", rng.IntN(1000), i),
				priority: int64(rng.IntN(4) * 100),
				// Few distinct instants, so creation ties are common and the
				// name tie-break is exercised.
				created: base.Add(time.Duration(rng.IntN(20)) * time.Second),
			}
		}
		// In-progress keys of the single tenant shift every round equally.
		inflight := make([]string, rng.IntN(5))
		if diff := gocmp.Diff(legacySelect(keys, limit), selectNames(keys, inflight, limit)); diff != "" {
			t.Fatalf("trial %d (n=%d, limit=%d): selection differs from the legacy order (-legacy +got):\n%s", trial, n, limit, diff)
		}
	}
}

func TestTenantSelectorOrder(t *testing.T) {
	base := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }

	// The incident: tenant a holds 1,000 older keys at priority 100 when
	// tenant b queues one newer key at the same priority.
	flood := make([]testKey, 0, 1001)
	for i := range 1000 {
		flood = append(flood, testKey{name: fmt.Sprintf("a/%04d", i), tenant: "a", priority: 100, created: at(i)})
	}
	flood = append(flood, testKey{name: "b/0", tenant: "b", priority: 100, created: at(5000)})

	tests := []struct {
		name     string
		keys     []testKey
		inflight []string
		limit    int
		want     []string
	}{{
		name:  "a newer tenant's key is enumerated past a flood",
		keys:  flood,
		limit: 10,
		want:  []string{"a/0000", "b/0", "a/0001", "a/0002", "a/0003", "a/0004", "a/0005", "a/0006", "a/0007", "a/0008"},
	}, {
		name:     "a tenant with work in flight goes behind an idle one",
		keys:     flood,
		inflight: []string{"a"},
		limit:    4,
		want:     []string{"b/0", "a/0000", "a/0001", "a/0002"},
	}, {
		name: "tenants alternate by round, oldest first within a round",
		keys: []testKey{
			{name: "a/1", tenant: "a", priority: 100, created: at(1)},
			{name: "a/2", tenant: "a", priority: 100, created: at(2)},
			{name: "a/3", tenant: "a", priority: 100, created: at(3)},
			{name: "b/1", tenant: "b", priority: 100, created: at(4)},
			{name: "c/1", tenant: "c", priority: 100, created: at(0)},
			{name: "b/2", tenant: "b", priority: 100, created: at(5)},
		},
		limit: 10,
		want:  []string{"c/1", "a/1", "b/1", "a/2", "b/2", "a/3"},
	}, {
		name: "priority outranks round",
		keys: []testKey{
			{name: "a/hi-1", tenant: "a", priority: 200, created: at(1)},
			{name: "a/hi-2", tenant: "a", priority: 200, created: at(2)},
			{name: "b/lo-1", tenant: "b", priority: 100, created: at(0)},
			{name: "a/lo-1", tenant: "a", priority: 100, created: at(3)},
		},
		inflight: []string{"a", "a"},
		limit:    10,
		want:     []string{"a/hi-1", "a/hi-2", "b/lo-1", "a/lo-1"},
	}, {
		name: "rounds restart at each priority",
		keys: []testKey{
			{name: "a/hi-1", tenant: "a", priority: 200, created: at(1)},
			{name: "a/hi-2", tenant: "a", priority: 200, created: at(2)},
			{name: "a/lo-1", tenant: "a", priority: 100, created: at(3)},
			{name: "b/lo-1", tenant: "b", priority: 100, created: at(4)},
		},
		limit: 10,
		want:  []string{"a/hi-1", "a/hi-2", "a/lo-1", "b/lo-1"},
	}, {
		name:  "a zero limit selects nothing",
		keys:  flood[:3],
		limit: 0,
		want:  []string{},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if diff := gocmp.Diff(tc.want, selectNames(tc.keys, tc.inflight, tc.limit)); diff != "" {
				t.Errorf("selection (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTenantSelectorBoundsRetention(t *testing.T) {
	const limit = 10
	s := NewTenantSelector[int](limit)
	base := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	for i := range 10_000 {
		tenant := fmt.Sprintf("t%d", i%3)
		s.Add(i, tenant, fmt.Sprintf("%s/%05d", tenant, i), int64(i%2), base.Add(time.Duration(i)*time.Second))
	}
	for tenant, entries := range s.byTenant {
		if len(entries) > 2*limit {
			t.Errorf("tenant %q retains %d keys, want at most %d", tenant, len(entries), 2*limit)
		}
	}
	if diff := gocmp.Diff(map[string]int{"t0": 3334, "t1": 3333, "t2": 3333}, s.Backlog()); diff != "" {
		t.Errorf("Backlog() (-want +got):\n%s", diff)
	}
	if got := len(s.Select()); got != limit {
		t.Errorf("Select() returned %d keys, want %d", got, limit)
	}
}

// uncappedSelect is the tenant-fair order with no retention cap: every key's
// round is computed over all of its tenant's keys at that priority before
// anything is truncated.
func uncappedSelect(keys []testKey, inflight []string, limit int) []string {
	running := map[string]int{}
	for _, tenant := range inflight {
		running[tenant]++
	}
	byCreation := func(a, b testKey) int {
		return cmp.Or(a.created.Compare(b.created), cmp.Compare(a.name, b.name))
	}
	type ranked struct {
		testKey
		round int
	}
	groups := map[string][]testKey{}
	for _, k := range keys {
		g := fmt.Sprintf("%s/%d", k.tenant, k.priority)
		groups[g] = append(groups[g], k)
	}
	var all []ranked
	for _, group := range groups {
		slices.SortFunc(group, byCreation)
		for i, k := range group {
			all = append(all, ranked{testKey: k, round: running[k.tenant] + i})
		}
	}
	slices.SortFunc(all, func(a, b ranked) int {
		return cmp.Or(
			cmp.Compare(b.priority, a.priority),
			cmp.Compare(a.round, b.round),
			byCreation(a.testKey, b.testKey),
		)
	})
	names := make([]string, 0, limit)
	for _, k := range all[:max(min(len(all), limit), 0)] {
		names = append(names, k.name)
	}
	return names
}

// TestTenantSelectorMatchesUncappedOrder pins that the per-tenant retention
// cap never drops a key that the uncapped order would select, across several
// tenants, priorities and in-progress counts.
func TestTenantSelectorMatchesUncappedOrder(t *testing.T) {
	base := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewPCG(3, 4))
	for trial := range 1000 {
		n := rng.IntN(300)
		limit := rng.IntN(20)
		tenants := 1 + rng.IntN(6)
		keys := make([]testKey, n)
		for i := range keys {
			keys[i] = testKey{
				name:     fmt.Sprintf("key-%d-%d", rng.IntN(1000), i),
				tenant:   fmt.Sprintf("t%d", rng.IntN(tenants)),
				priority: int64(rng.IntN(3) * 100),
				created:  base.Add(time.Duration(rng.IntN(20)) * time.Second),
			}
		}
		inflight := make([]string, rng.IntN(8))
		for i := range inflight {
			inflight[i] = fmt.Sprintf("t%d", rng.IntN(tenants))
		}
		if diff := gocmp.Diff(uncappedSelect(keys, inflight, limit), selectNames(keys, inflight, limit)); diff != "" {
			t.Fatalf("trial %d (n=%d, limit=%d, tenants=%d): selection differs from the uncapped order (-uncapped +got):\n%s", trial, n, limit, tenants, diff)
		}
	}
}
