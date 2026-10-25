/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"chainguard.dev/driftlessaf/workqueue"
)

// legacyOrderCandidates is orderCandidates as it was before tenants existed:
// the head of each priority run is shuffled as one group.
func legacyOrderCandidates(next []workqueue.QueuedKey, active map[string]struct{}, window int, shuffle func(n int, swap func(i, j int))) []workqueue.QueuedKey {
	if shuffle == nil || window <= 0 {
		return next
	}
	eligible := make([]workqueue.QueuedKey, 0, len(next))
	for _, key := range next {
		if _, ok := active[key.Name()]; ok {
			continue
		}
		eligible = append(eligible, key)
	}
	for start := 0; start < len(eligible); {
		end := start + 1
		for end < len(eligible) && eligible[end].Priority() == eligible[start].Priority() {
			end++
		}
		if n := min(window, end-start); n > 1 {
			run := eligible[start : start+n]
			shuffle(n, func(i, j int) { run[i], run[j] = run[j], run[i] })
		}
		start = end
	}
	return eligible
}

// TestOrderCandidates_WithoutTenantsMatchesLegacy pins that keys which do not
// implement workqueue.TenantKey are ordered exactly as before tenants existed,
// down to the shuffle source's draws.
func TestOrderCandidates_WithoutTenantsMatchesLegacy(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for trial := range uint64(500) {
		next := make([]workqueue.QueuedKey, rng.IntN(120))
		active := map[string]struct{}{}
		for i := range next {
			name := fmt.Sprintf("k%d", i)
			next[i] = &mockKey{name: name, priority: int64(rng.IntN(3) * 100)}
			if rng.IntN(10) == 0 {
				active[name] = struct{}{}
			}
		}
		slices.SortStableFunc(next, func(a, b workqueue.QueuedKey) int { return cmp.Compare(b.Priority(), a.Priority()) })
		window := rng.IntN(40)
		// Tenant counts have no effect on keys without a tenant.
		inflight := map[string]int{"": rng.IntN(4)}

		want := keyNames(legacyOrderCandidates(slices.Clone(next), active, window, seededShuffle(trial)))
		got := keyNames(orderCandidates(slices.Clone(next), active, inflight, window, seededShuffle(trial)))
		if diff := gocmp.Diff(want, got); diff != "" {
			t.Fatalf("trial %d: order differs from the legacy order (-legacy +got):\n%s", trial, diff)
		}
	}
}

type tenantMockKey struct {
	*mockKey
	tenant string
}

func (k *tenantMockKey) Tenant() string { return k.tenant }

func tenantKeys(names ...string) []workqueue.QueuedKey {
	keys := make([]workqueue.QueuedKey, 0, len(names))
	for _, name := range names {
		keys = append(keys, &tenantMockKey{mockKey: &mockKey{name: name, priority: 100}, tenant: name[:1]})
	}
	return keys
}

func TestOrderCandidates_ShufflesWithinTenantRounds(t *testing.T) {
	tests := []struct {
		name     string
		next     []workqueue.QueuedKey
		inflight map[string]int
		// groups lists, in order, the sets of keys that may be permuted among
		// themselves.
		groups [][]string
	}{{
		name:   "rounds of three tenants",
		next:   tenantKeys("a1", "b1", "c1", "a2", "b2", "a3"),
		groups: [][]string{{"a1", "b1", "c1"}, {"a2", "b2"}, {"a3"}},
	}, {
		name:     "a busy tenant's keys start in a later round",
		next:     tenantKeys("b1", "c1", "a1", "b2", "a2", "a3"),
		inflight: map[string]int{"a": 1},
		groups:   [][]string{{"b1", "c1"}, {"a1", "b2"}, {"a2", "a3"}},
	}, {
		name:   "the tail of a tenant flood is one group",
		next:   tenantKeys("a1", "b1", "c1", "m1", "m2", "m3", "m4", "m5"),
		groups: [][]string{{"a1", "b1", "c1", "m1"}, {"m2", "m3", "m4", "m5"}},
	}, {
		name:     "one-key rounds of different tenants stay in order",
		next:     tenantKeys("a1", "a2", "b1", "c1", "d1", "d2"),
		inflight: map[string]int{"b": 2, "c": 3, "d": 4},
		groups:   [][]string{{"a1", "a2"}, {"b1"}, {"c1"}, {"d1", "d2"}},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			moved := make([]bool, len(tc.groups))
			for seed := range uint64(50) {
				got := keyNames(orderCandidates(tc.next, nil, tc.inflight, 32, seededShuffle(seed)))
				for i, group := range tc.groups {
					head := got[:len(group)]
					got = got[len(group):]
					if diff := gocmp.Diff(slices.Sorted(slices.Values(group)), slices.Sorted(slices.Values(head))); diff != "" {
						t.Fatalf("seed %d: group (-want +got):\n%s", seed, diff)
					}
					if !slices.Equal(group, head) {
						moved[i] = true
					}
				}
			}
			for i, group := range tc.groups {
				if len(group) > 1 && !moved[i] {
					t.Errorf("no seed permuted group %v", group)
				}
			}
		})
	}
}

// lostClaimKey fails Start the way a claim lost to another dispatcher does.
type lostClaimKey struct {
	*mockKey
	err      error
	attempts *int
	mu       *sync.Mutex
}

func (k *lostClaimKey) Start(context.Context) (workqueue.OwnedInProgressKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	*k.attempts++
	return nil, k.err
}

func TestHandleAsync_LostClaimFallsThrough(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantStarted  []string
		wantAttempts int
	}{{
		name:         "a lost claim tries the next candidate",
		err:          fmt.Errorf("%w: precondition failed", workqueue.ErrClaimLost),
		wantStarted:  []string{"k2"},
		wantAttempts: 2,
	}, {
		name:         "any other Start error ends the slot's pass",
		err:          errors.New("server error"),
		wantAttempts: 1,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			attempts := 0
			next := []workqueue.QueuedKey{
				&lostClaimKey{mockKey: &mockKey{name: "k0"}, err: tc.err, attempts: &attempts, mu: &mu},
				&lostClaimKey{mockKey: &mockKey{name: "k1"}, err: tc.err, attempts: &attempts, mu: &mu},
				&mockKey{name: "k2"},
				&mockKey{name: "k3"},
			}
			q := &mockQueue{next: next}

			var started []string
			future := HandleAsync(t.Context(), q, 1, 0, func(_ context.Context, key string, _ workqueue.Options) error {
				started = append(started, key)
				return nil
			}, 0)
			if err := future(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := gocmp.Diff(tc.wantStarted, started); diff != "" {
				t.Errorf("started keys (-want +got):\n%s", diff)
			}
			if attempts != tc.wantAttempts {
				t.Errorf("failed claims: got = %d, want = %d", attempts, tc.wantAttempts)
			}
		})
	}
}

func TestHandleAsync_LostClaimSettlesOncePerSlot(t *testing.T) {
	lost := fmt.Errorf("%w: precondition failed", workqueue.ErrClaimLost)
	tests := []struct {
		name        string
		next        func(attempts *int, mu *sync.Mutex) []workqueue.QueuedKey
		wantSettled []bool
	}{{
		name: "first claim lost, second claim succeeds",
		next: func(attempts *int, mu *sync.Mutex) []workqueue.QueuedKey {
			return []workqueue.QueuedKey{
				&lostClaimKey{mockKey: &mockKey{name: "k0"}, err: lost, attempts: attempts, mu: mu},
				&mockKey{name: "k1"},
			}
		},
		wantSettled: []bool{true},
	}, {
		name: "every claim lost",
		next: func(attempts *int, mu *sync.Mutex) []workqueue.QueuedKey {
			return []workqueue.QueuedKey{
				&lostClaimKey{mockKey: &mockKey{name: "k0"}, err: lost, attempts: attempts, mu: mu},
				&lostClaimKey{mockKey: &mockKey{name: "k1"}, err: lost, attempts: attempts, mu: mu},
			}
		},
		wantSettled: []bool{false},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			attempts := 0
			q := &mockQueue{next: tc.next(&attempts, &mu)}

			var hooks sync.Mutex
			var launched []int
			var settled []bool
			future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
				return nil
			}, 0, WithLaunchHooks(
				func(n int) {
					hooks.Lock()
					defer hooks.Unlock()
					launched = append(launched, n)
				},
				func(started bool) {
					hooks.Lock()
					defer hooks.Unlock()
					settled = append(settled, started)
				}))
			if err := future(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := gocmp.Diff([]int{1}, launched); diff != "" {
				t.Errorf("launched (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tc.wantSettled, settled); diff != "" {
				t.Errorf("settled (-want +got):\n%s", diff)
			}
		})
	}
}
