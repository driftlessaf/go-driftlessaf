/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue

import (
	"cmp"
	"slices"
	"time"
)

// TenantFunc returns the tenant a key belongs to, e.g. the GitHub account that
// owns the resource the key names. A queue configured with one orders the keys
// of each priority so that no tenant's backlog hides another tenant's keys.
type TenantFunc func(key string) string

// TenantKey is implemented by the queued and in-progress keys of a queue
// configured with a TenantFunc. Keys of a queue without one do not implement
// it, which is how a dispatcher tells the two apart.
type TenantKey interface {
	// Tenant returns the tenant the TenantFunc assigned to the key.
	Tenant() string
}

// TenantSelector picks the next limit queued keys in tenant-fair order:
// priority descending, then round ascending, then creation time and name
// ascending. A key's round is its tenant's in-progress count plus the key's
// index among that tenant's eligible keys at the same priority, so each
// tenant's oldest key goes first and tenants with less work running go ahead
// of busy ones.
//
// When every key has the same tenant, round grows with creation time within
// a priority, and the order is exactly priority, then creation time, then
// name.
//
// A TenantSelector is not safe for concurrent use.
type TenantSelector[K any] struct {
	limit    int
	inflight map[string]int
	byTenant map[string][]tenantEntry[K]
	backlog  map[string]int
}

type tenantEntry[K any] struct {
	key      K
	name     string
	priority int64
	created  time.Time
	round    int
}

// NewTenantSelector returns a selector that keeps at most limit keys.
func NewTenantSelector[K any](limit int) *TenantSelector[K] {
	return &TenantSelector[K]{
		limit:    limit,
		inflight: map[string]int{},
		byTenant: map[string][]tenantEntry[K]{},
		backlog:  map[string]int{},
	}
}

// InProgress counts one live in-progress key against tenant.
func (s *TenantSelector[K]) InProgress(tenant string) {
	s.inflight[tenant]++
}

// Add offers an eligible queued key.
func (s *TenantSelector[K]) Add(key K, tenant, name string, priority int64, created time.Time) {
	s.backlog[tenant]++
	if s.limit <= 0 {
		return
	}
	// Only each tenant's first limit keys in (priority, creation) order can
	// make the selection: a later key has at least limit keys of its own tenant
	// ahead of it, either at a higher priority or at the same priority with a
	// lower round. Compacting at twice the limit keeps Add amortized O(log n).
	s.byTenant[tenant] = append(s.byTenant[tenant], tenantEntry[K]{key: key, name: name, priority: priority, created: created})
	if len(s.byTenant[tenant]) > 2*s.limit {
		s.byTenant[tenant] = keepFirst(s.byTenant[tenant], s.limit)
	}
}

// Backlog returns the number of eligible keys offered per tenant, including
// those Select drops.
func (s *TenantSelector[K]) Backlog() map[string]int {
	return s.backlog
}

// Select returns at most limit keys in tenant-fair order.
func (s *TenantSelector[K]) Select() []K {
	var all []tenantEntry[K]
	for tenant, entries := range s.byTenant {
		entries = keepFirst(entries, s.limit)
		k := 0
		for i := range entries {
			if i > 0 && entries[i].priority != entries[i-1].priority {
				k = 0
			}
			entries[i].round = s.inflight[tenant] + k
			k++
		}
		all = append(all, entries...)
	}
	slices.SortFunc(all, func(a, b tenantEntry[K]) int {
		return cmp.Or(
			cmp.Compare(b.priority, a.priority),
			cmp.Compare(a.round, b.round),
			a.created.Compare(b.created),
			cmp.Compare(a.name, b.name),
		)
	})
	n := max(min(len(all), s.limit), 0)
	keys := make([]K, 0, n)
	for _, e := range all[:n] {
		keys = append(keys, e.key)
	}
	return keys
}

// keepFirst sorts entries by priority descending, then creation time and name
// ascending, and truncates them to n.
func keepFirst[K any](entries []tenantEntry[K], n int) []tenantEntry[K] {
	slices.SortFunc(entries, func(a, b tenantEntry[K]) int {
		return cmp.Or(
			cmp.Compare(b.priority, a.priority),
			a.created.Compare(b.created),
			cmp.Compare(a.name, b.name),
		)
	})
	return entries[:min(len(entries), n)]
}
