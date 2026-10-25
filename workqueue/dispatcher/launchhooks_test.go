/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/driftlessaf/workqueue/dispatcher"
	"chainguard.dev/driftlessaf/workqueue/inmem"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// launchRecorder collects WithLaunchHooks calls.
type launchRecorder struct {
	mu       sync.Mutex
	launched []int
	settled  []bool
}

func (r *launchRecorder) option(onSettled func(started bool)) dispatcher.Option {
	return dispatcher.WithLaunchHooks(
		func(n int) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.launched = append(r.launched, n)
		},
		func(started bool) {
			if onSettled != nil {
				onSettled(started)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			r.settled = append(r.settled, started)
		})
}

func TestLaunchHooks_BatchLaunchesSeveralKeys(t *testing.T) {
	ctx := t.Context()
	wq := inmem.NewWorkQueue(10)
	for i := range 5 {
		if err := wq.Queue(ctx, fmt.Sprint(i), workqueue.Options{Priority: int64(5 - i)}); err != nil {
			t.Fatal(err)
		}
	}
	var rec launchRecorder
	future := dispatcher.HandleAsync(ctx, wq, 10, 3, func(context.Context, string, workqueue.Options) error {
		return nil
	}, 0, rec.option(nil))
	rec.mu.Lock()
	if got, want := fmt.Sprint(rec.launched), "[3]"; got != want {
		t.Errorf("launched before HandleAsync returned: got = %s, want = %s", got, want)
	}
	rec.mu.Unlock()
	if err := future(); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(rec.settled), "[true true true]"; got != want {
		t.Errorf("settled: got = %s, want = %s", got, want)
	}
	for _, key := range []string{"3", "4"} {
		if state, err := wq.Get(ctx, key); err != nil || state.GetStatus() != workqueue.KeyState_QUEUED {
			t.Errorf("key %q beyond the batch: got = %v, %v, want = queued", key, state, err)
		}
	}
}

func TestLaunchHooks_NothingToLaunch(t *testing.T) {
	var rec launchRecorder
	if err := dispatcher.Handle(t.Context(), inmem.NewWorkQueue(10), 10, 3, func(context.Context, string, workqueue.Options) error {
		t.Error("callback invoked for empty queue")
		return nil
	}, rec.option(nil)); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(rec.launched), "[0]"; got != want {
		t.Errorf("launched: got = %s, want = %s", got, want)
	}
	if len(rec.settled) != 0 {
		t.Errorf("settled: got = %v, want = none", rec.settled)
	}
}

// siblingClaims claims every key it lists through the real queue before
// returning the listing, as a sibling dispatcher winning each race between
// this dispatcher's listing and its Start would.
type siblingClaims struct {
	workqueue.Interface
}

func (q siblingClaims) Enumerate(ctx context.Context) ([]workqueue.ObservedInProgressKey, []workqueue.QueuedKey, []workqueue.DeadLetteredKey, error) {
	wip, next, dead, err := q.Interface.Enumerate(ctx)
	for _, key := range next {
		if _, err := key.Start(ctx); err != nil {
			return nil, nil, nil, fmt.Errorf("sibling claim of %q: %w", key.Name(), err)
		}
	}
	return wip, next, dead, err
}

func TestLaunchHooks_SettledOncePerKeyAfterCleanup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		lostRace    bool
		callbackErr error
		wantStarted bool
		// wantState is the key's status when settled fires, or UNKNOWN for a
		// key that is gone because it completed.
		wantState workqueue.KeyState_Status
	}{
		{name: "success", wantStarted: true, wantState: workqueue.KeyState_UNKNOWN},
		{name: "failure", callbackErr: errors.New("transient"), wantStarted: true, wantState: workqueue.KeyState_QUEUED},
		{name: "start failure", lostRace: true, wantStarted: false, wantState: workqueue.KeyState_IN_PROGRESS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			wq := inmem.NewWorkQueue(10)
			if err := wq.Queue(ctx, "key", workqueue.Options{}); err != nil {
				t.Fatal(err)
			}
			if tc.lostRace {
				wq = siblingClaims{Interface: wq}
			}
			var rec launchRecorder
			// Observe the queue from inside the hook, so the assertion is on the
			// state when the slot is released rather than after the pass.
			var observed workqueue.KeyState_Status
			onSettled := func(bool) {
				state, err := wq.Get(context.WithoutCancel(ctx), "key")
				switch {
				case status.Code(err) == codes.NotFound:
					observed = workqueue.KeyState_UNKNOWN
				case err != nil:
					t.Errorf("Get() in settled hook: %v", err)
				default:
					observed = state.GetStatus()
				}
			}
			calls := 0
			if err := dispatcher.Handle(ctx, wq, 10, 5, func(context.Context, string, workqueue.Options) error {
				calls++
				return tc.callbackErr
			}, rec.option(onSettled)); err != nil {
				t.Fatal(err)
			}
			if got, want := fmt.Sprint(rec.launched), "[1]"; got != want {
				t.Errorf("launched: got = %s, want = %s", got, want)
			}
			if got, want := fmt.Sprint(rec.settled), fmt.Sprintf("[%t]", tc.wantStarted); got != want {
				t.Errorf("settled: got = %s, want = %s", got, want)
			}
			if wantCalls := map[bool]int{true: 1, false: 0}[tc.wantStarted]; calls != wantCalls {
				t.Errorf("callback calls: got = %d, want = %d", calls, wantCalls)
			}
			if observed != tc.wantState {
				t.Errorf("key state when settled fired: got = %v, want = %v", observed, tc.wantState)
			}
		})
	}
}
