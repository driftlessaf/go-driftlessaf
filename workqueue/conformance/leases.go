/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package conformance

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
)

// TestLeases verifies how a durable workqueue leases in-progress keys: a
// heartbeat keeps a running key's lease alive, a key whose owner stopped
// heartbeating is orphaned and can be recovered by another dispatcher, and a
// dispatcher acting on a lease it no longer holds, or on a stale view of one,
// changes nothing. It also covers the capacity-aware listings, whose answer
// depends on which leases are live.
//
// ctor must return a queue over the same storage on every call, recording
// identity as the owner of the keys it starts, so that separate instances
// stand in for separate dispatchers. lease is the longest a started key stays
// live without a heartbeat. Configure the implementation so that a key whose
// owner stopped heartbeating is orphaned within lease, and so that a live
// owner renews its lease more than once within lease. The suite stops an
// owner by cancelling the context it passed to Start.
func TestLeases(t *testing.T, lease time.Duration, ctor func(identity string) workqueue.Interface) {
	lt := &leaseTester{t: t, lease: lease, ctor: ctor}

	lt.scenario("a heartbeat keeps a running key's lease alive", func(ctx context.Context, t *testing.T) {
		a := lt.queue(t, "a")
		owned := lt.start(ctx, t, a, ctx, "foo")

		// Without its heartbeat, the key would be orphaned by now.
		time.Sleep(2 * lt.lease)

		if lt.inProgress(t, a, "foo").IsOrphaned() {
			t.Error("IsOrphaned() of a heartbeated key: got = true, want = false")
		}
		if err := owned.Context().Err(); err != nil {
			t.Errorf("owner context of a heartbeated key: got = %v, want = nil", err)
		}
		if err := owned.Complete(ctx); err != nil {
			t.Fatalf("Complete failed: %v", err)
		}
		_, _ = checkQueue(t, a, ExpectedState{})
	})

	lt.scenario("an abandoned key is orphaned and another dispatcher recovers it", func(ctx context.Context, t *testing.T) {
		a := lt.queue(t, "a")
		lt.abandon(ctx, t, a, "foo")

		b := lt.queue(t, "b")
		if err := lt.waitOrphaned(t, b, "foo").Requeue(ctx); err != nil {
			t.Fatalf("Requeue of the orphan failed: %v", err)
		}
		_, qd := checkQueue(t, b, ExpectedState{Queued: []string{"foo"}})

		owned, err := qd[0].Start(ctx)
		if err != nil {
			t.Fatalf("Start of the recovered key failed: %v", err)
		}
		// The abandoned run counts as an attempt.
		if got, want := owned.GetAttempts(), 2; got != want {
			t.Errorf("GetAttempts() of the recovered key: got = %d, want = %d", got, want)
		}
		if got := lt.inProgress(t, b, "foo").Owner(); got != "b" {
			t.Errorf("Owner() of the recovered key: got = %q, want = %q", got, "b")
		}
		if err := owned.Complete(ctx); err != nil {
			t.Fatalf("Complete failed: %v", err)
		}
		_, _ = checkQueue(t, b, ExpectedState{})
	})

	lt.scenario("a dispatcher that lost a key cannot settle it", func(ctx context.Context, t *testing.T) {
		a := lt.queue(t, "a")
		stale := lt.abandon(ctx, t, a, "foo")

		b := lt.queue(t, "b")
		owned := lt.recover(ctx, t, b, "foo")

		if err := stale.Complete(ctx); err != nil {
			t.Logf("stale Complete returned %v", err)
		}
		lt.checkHeldBy(t, b, "b")

		if err := stale.Requeue(ctx); err != nil {
			t.Logf("stale Requeue returned %v", err)
		}
		lt.checkHeldBy(t, b, "b")

		if err := stale.Deadletter(ctx); !errors.Is(err, workqueue.ErrDeadletterSkipped) {
			t.Errorf("stale Deadletter: got = %v, want = an error wrapping workqueue.ErrDeadletterSkipped", err)
		}
		lt.checkHeldBy(t, b, "b")

		if err := owned.Complete(ctx); err != nil {
			t.Fatalf("Complete failed: %v", err)
		}
		_, _ = checkQueue(t, b, ExpectedState{})
	})

	lt.scenario("a stale view of a renewed lease cannot requeue or dead-letter the key", func(ctx context.Context, t *testing.T) {
		a := lt.queue(t, "a")
		owned := lt.start(ctx, t, a, ctx, "foo")

		b := lt.queue(t, "b")
		observed := lt.inProgress(t, b, "foo")

		// Outlast the lease b observed. A key still live afterwards has had
		// its lease renewed since b listed it.
		time.Sleep(lt.lease + lt.lease/2)
		lt.checkHeldBy(t, b, "a")

		if err := observed.Requeue(ctx); err != nil {
			t.Logf("stale observed Requeue returned %v", err)
		}
		lt.checkHeldBy(t, b, "a")

		checkObservedDeadletterSkipped(ctx, t, observed)
		lt.checkHeldBy(t, b, "a")

		if err := owned.Context().Err(); err != nil {
			t.Errorf("owner context after stale observed settles: got = %v, want = nil", err)
		}
		if err := owned.Complete(ctx); err != nil {
			t.Fatalf("Complete failed: %v", err)
		}
		_, _ = checkQueue(t, a, ExpectedState{})
	})

	lt.scenario("a stale view of a recovered key cannot requeue or dead-letter it", func(ctx context.Context, t *testing.T) {
		a := lt.queue(t, "a")
		lt.abandon(ctx, t, a, "foo")

		b := lt.queue(t, "b")
		observed := lt.waitOrphaned(t, b, "foo")

		c := lt.queue(t, "c")
		owned := lt.recover(ctx, t, c, "foo")

		if err := observed.Requeue(ctx); err != nil {
			t.Logf("stale observed Requeue returned %v", err)
		}
		lt.checkHeldBy(t, c, "c")

		checkObservedDeadletterSkipped(ctx, t, observed)
		lt.checkHeldBy(t, c, "c")

		if err := owned.Complete(ctx); err != nil {
			t.Fatalf("Complete failed: %v", err)
		}
		_, _ = checkQueue(t, c, ExpectedState{})
	})

	lt.scenario("a capacity-aware listing reads the backlog once a slot is free", func(ctx context.Context, t *testing.T) {
		a := lt.queue(t, "a")
		bounded, ok := a.(workqueue.CapacityAware)
		if !ok {
			t.Skip("the workqueue does not implement workqueue.CapacityAware")
		}
		startCtx, stop := context.WithCancel(ctx)
		defer stop()
		running := lt.start(ctx, t, a, startCtx, "running")
		if err := a.Queue(ctx, "waiting", workqueue.Options{}); err != nil {
			t.Fatalf("Queue failed: %v", err)
		}

		// Full: every in-progress key is listed, the backlog may be skipped.
		wip, _, _, err := bounded.EnumerateWithCapacity(ctx, 1)
		if err != nil {
			t.Fatalf("EnumerateWithCapacity failed: %v", err)
		}
		checkNames(t, "in-progress keys at capacity", keyNames(wip), []string{"running"})

		// A free slot: the backlog is listed.
		wip, qd, _, err := bounded.EnumerateWithCapacity(ctx, 2)
		if err != nil {
			t.Fatalf("EnumerateWithCapacity failed: %v", err)
		}
		checkNames(t, "in-progress keys below capacity", keyNames(wip), []string{"running"})
		checkNames(t, "queued keys below capacity", keyNames(qd), []string{"waiting"})

		// An orphan holds no slot, so its expiry frees one.
		stop()
		lt.waitOrphaned(t, a, "running")
		wip, qd, _, err = bounded.EnumerateWithCapacity(ctx, 1)
		if err != nil {
			t.Fatalf("EnumerateWithCapacity failed: %v", err)
		}
		checkNames(t, "in-progress keys with an orphan", keyNames(wip), []string{"running"})
		checkNames(t, "queued keys with an orphan", keyNames(qd), []string{"waiting"})

		if err := running.Complete(ctx); err != nil {
			t.Logf("Complete of the orphan's handle returned %v", err)
		}
	})

	lt.scenario("an owner-capacity-aware listing reads the backlog for an owner below its share", func(ctx context.Context, t *testing.T) {
		a := lt.queue(t, "a")
		b := lt.queue(t, "b")
		ownedA, okA := a.(workqueue.OwnerCapacityAware)
		ownedB, okB := b.(workqueue.OwnerCapacityAware)
		if !okA || !okB {
			t.Skip("the workqueue does not implement workqueue.OwnerCapacityAware")
		}
		running := lt.start(ctx, t, a, ctx, "running")
		if err := a.Queue(ctx, "waiting", workqueue.Options{}); err != nil {
			t.Fatalf("Queue failed: %v", err)
		}

		// a holds its whole share: every in-progress key is listed, the
		// backlog may be skipped.
		wip, _, _, err := ownedA.EnumerateWithOwnerCapacity(ctx, 10, 1)
		if err != nil {
			t.Fatalf("EnumerateWithOwnerCapacity failed: %v", err)
		}
		checkNames(t, "in-progress keys for an owner at its share", keyNames(wip), []string{"running"})

		// b holds nothing, so its listing includes the backlog.
		wip, qd, _, err := ownedB.EnumerateWithOwnerCapacity(ctx, 10, 1)
		if err != nil {
			t.Fatalf("EnumerateWithOwnerCapacity failed: %v", err)
		}
		checkNames(t, "in-progress keys for an owner below its share", keyNames(wip), []string{"running"})
		checkNames(t, "queued keys for an owner below its share", keyNames(qd), []string{"waiting"})

		if err := running.Complete(ctx); err != nil {
			t.Fatalf("Complete failed: %v", err)
		}
	})
}

type leaseTester struct {
	t     *testing.T
	lease time.Duration
	ctor  func(identity string) workqueue.Interface
}

// scenario runs f against storage drained before and after it.
func (lt *leaseTester) scenario(name string, f func(context.Context, *testing.T)) {
	lt.t.Run(name, func(t *testing.T) {
		wq := lt.queue(t, "")
		if err := drain(t, wq); err != nil {
			t.Fatalf("Drain failed: %v", err)
		}
		_, _ = checkQueue(t, wq, ExpectedState{})
		t.Cleanup(func() {
			if err := drain(t, wq); err != nil {
				t.Fatalf("Drain failed: %v", err)
			}
			_, _ = checkQueue(t, wq, ExpectedState{})
		})
		f(t.Context(), t)
	})
}

func (lt *leaseTester) queue(t *testing.T, identity string) workqueue.Interface {
	t.Helper()
	wq := lt.ctor(identity)
	if wq == nil {
		t.Fatal("NewWorkQueue returned nil")
	}
	return wq
}

// start queues key and starts it under startCtx, whose cancellation stops the
// owner's heartbeat.
func (lt *leaseTester) start(ctx context.Context, t *testing.T, wq workqueue.Interface, startCtx context.Context, key string) workqueue.OwnedInProgressKey {
	t.Helper()
	if err := wq.Queue(ctx, key, workqueue.Options{}); err != nil {
		t.Fatalf("Queue failed: %v", err)
	}
	_, qd, _, err := wq.Enumerate(ctx)
	if err != nil {
		t.Fatalf("Enumerate failed: %v", err)
	}
	for _, k := range qd {
		if k.Name() != key {
			continue
		}
		owned, err := k.Start(startCtx)
		if err != nil {
			t.Fatalf("Start(%q) failed: %v", key, err)
		}
		return owned
	}
	t.Fatalf("queued keys: got = %v, want %q among them", keyNames(qd), key)
	return nil
}

// abandon starts key and then stops its owner, as a dispatcher that dies
// mid-run would, and returns the abandoned owner's handle.
func (lt *leaseTester) abandon(ctx context.Context, t *testing.T, wq workqueue.Interface, key string) workqueue.OwnedInProgressKey {
	t.Helper()
	startCtx, stop := context.WithCancel(ctx)
	owned := lt.start(ctx, t, wq, startCtx, key)
	stop()
	return owned
}

// recover waits for key to be orphaned, requeues it and starts it on wq.
func (lt *leaseTester) recover(ctx context.Context, t *testing.T, wq workqueue.Interface, key string) workqueue.OwnedInProgressKey {
	t.Helper()
	if err := lt.waitOrphaned(t, wq, key).Requeue(ctx); err != nil {
		t.Fatalf("Requeue of the orphan failed: %v", err)
	}
	_, qd := checkQueue(t, wq, ExpectedState{Queued: []string{key}})
	owned, err := qd[0].Start(ctx)
	if err != nil {
		t.Fatalf("Start of the recovered key failed: %v", err)
	}
	return owned
}

// waitOrphaned lists wq until key is in progress and orphaned, and fails the
// test if that takes well past the lease.
func (lt *leaseTester) waitOrphaned(t *testing.T, wq workqueue.Interface, key string) workqueue.ObservedInProgressKey {
	t.Helper()
	deadline := time.Now().Add(3 * lt.lease)
	for {
		k := lt.inProgress(t, wq, key)
		if k.IsOrphaned() {
			return k
		}
		if time.Now().After(deadline) {
			t.Fatalf("key %q: still not orphaned %v after its owner stopped, want orphaned within the lease of %v", key, 3*lt.lease, lt.lease)
		}
		time.Sleep(lt.lease / 10)
	}
}

// inProgress lists wq and returns the in-progress key named key.
func (lt *leaseTester) inProgress(t *testing.T, wq workqueue.Interface, key string) workqueue.ObservedInProgressKey {
	t.Helper()
	wip, _, _, err := wq.Enumerate(t.Context())
	if err != nil {
		t.Fatalf("Enumerate failed: %v", err)
	}
	for _, k := range wip {
		if k.Name() == key {
			return k
		}
	}
	t.Fatalf("in-progress keys: got = %v, want %q among them", keyNames(wip), key)
	return nil
}

// checkHeldBy checks that "foo" is the only key in wq, in progress under a
// live lease recorded for owner.
func (lt *leaseTester) checkHeldBy(t *testing.T, wq workqueue.Interface, owner string) {
	t.Helper()
	const key = "foo"
	wip, _ := checkQueue(t, wq, ExpectedState{WorkInProgress: []string{key}})
	if wip[0].IsOrphaned() {
		t.Errorf("IsOrphaned() of %q: got = true, want = false", key)
	}
	if got := wip[0].Owner(); got != owner {
		t.Errorf("Owner() of %q: got = %q, want = %q", key, got, owner)
	}
}

func keyNames[K workqueue.Key](keys []K) []string {
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, k.Name())
	}
	return names
}

func checkNames(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s: got = %v, want = %v", what, got, want)
	}
}

// observedDeadletter is the optional surface a dispatcher's orphan sweep uses
// to dead-letter a key it only observed.
type observedDeadletter interface {
	Deadletter(context.Context) error
}

// checkObservedDeadletterSkipped checks that dead-lettering a stale
// observation reports ErrDeadletterSkipped, when the queue's observed keys can
// be dead-lettered at all.
func checkObservedDeadletterSkipped(ctx context.Context, t *testing.T, observed workqueue.ObservedInProgressKey) {
	t.Helper()
	d, ok := observed.(observedDeadletter)
	if !ok {
		return
	}
	if err := d.Deadletter(ctx); !errors.Is(err, workqueue.ErrDeadletterSkipped) {
		t.Errorf("stale observed Deadletter: got = %v, want = an error wrapping workqueue.ErrDeadletterSkipped", err)
	}
}
