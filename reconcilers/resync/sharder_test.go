/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package resync_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/resync"
	"chainguard.dev/driftlessaf/workqueue"
	"google.golang.org/grpc"
)

// staticNow returns a clock function that always reports t. Suitable for the
// majority of tests, which exercise shard math without latency compensation.
func staticNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// fakeClock is a manually-advanced clock whose Now method is safe for
// concurrent use. Tests use it to simulate elapsed time between Sharder
// construction and individual enqueue calls.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeWQ records each (key, delay) it observes via Process. Optionally
// returns processErr from every Process call.
type fakeWQ struct {
	workqueue.WorkqueueServiceClient

	mu         sync.Mutex
	enqueued   map[string]int64
	processErr error
}

func (f *fakeWQ) Process(_ context.Context, req *workqueue.ProcessRequest, _ ...grpc.CallOption) (*workqueue.ProcessResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.processErr != nil {
		return nil, f.processErr
	}
	if f.enqueued == nil {
		f.enqueued = make(map[string]int64)
	}
	f.enqueued[req.GetKey()] = req.GetDelaySeconds()
	return &workqueue.ProcessResponse{}, nil
}

func (f *fakeWQ) Close() error { return nil }

func TestNewValidation(t *testing.T) {
	now := staticNow(time.Unix(1700000000, 0))
	cases := []struct {
		name    string
		tick    time.Duration
		wq      workqueue.Client
		opts    []resync.Option
		wantErr bool
	}{{
		name: "valid (defaults to time.Now when WithNow omitted)",
		tick: time.Hour,
		wq:   &fakeWQ{},
	}, {
		name:    "WithNow nil rejected",
		tick:    time.Hour,
		wq:      &fakeWQ{},
		opts:    []resync.Option{resync.WithNow(nil)},
		wantErr: true,
	}, {
		name:    "tick zero",
		tick:    0,
		wq:      &fakeWQ{},
		opts:    []resync.Option{resync.WithNow(now)},
		wantErr: true,
	}, {
		name:    "tick negative",
		tick:    -time.Hour,
		wq:      &fakeWQ{},
		opts:    []resync.Option{resync.WithNow(now)},
		wantErr: true,
	}, {
		name:    "tick not whole minute",
		tick:    30 * time.Second,
		wq:      &fakeWQ{},
		opts:    []resync.Option{resync.WithNow(now)},
		wantErr: true,
	}, {
		name:    "wq nil",
		tick:    time.Hour,
		wq:      nil,
		opts:    []resync.Option{resync.WithNow(now)},
		wantErr: true,
	}, {
		name:    "concurrency zero",
		tick:    time.Hour,
		wq:      &fakeWQ{},
		opts:    []resync.Option{resync.WithNow(now), resync.WithConcurrency(0)},
		wantErr: true,
	}, {
		name:    "concurrency negative",
		tick:    time.Hour,
		wq:      &fakeWQ{},
		opts:    []resync.Option{resync.WithNow(now), resync.WithConcurrency(-1)},
		wantErr: true,
	}, {
		name: "concurrency positive",
		tick: time.Hour,
		wq:   &fakeWQ{},
		opts: []resync.Option{resync.WithNow(now), resync.WithConcurrency(8)},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resync.New(tc.tick, tc.wq, tc.opts...)
			if (err != nil) != tc.wantErr {
				t.Errorf("New: got = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestProcessValidation(t *testing.T) {
	sh, err := resync.New(time.Hour, &fakeWQ{}, resync.WithNow(staticNow(time.Unix(1700000000, 0))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		name    string
		period  time.Duration
		wantErr bool
	}{
		{name: "valid", period: 24 * time.Hour},
		{name: "period equals tick", period: time.Hour},
		{name: "period zero", period: 0, wantErr: true},
		{name: "period negative", period: -time.Hour, wantErr: true},
		{name: "period not multiple of tick", period: 90 * time.Minute, wantErr: true},
		{name: "period smaller than tick", period: 30 * time.Minute, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sh.Process(t.Context(), tc.period, nil)
			if (err != nil) != tc.wantErr {
				t.Errorf("Process: got = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestProcessCoverage iterates one full period worth of ticks and asserts
// that every key is enqueued exactly once, with a delay in [0, tick).
func TestProcessCoverage(t *testing.T) {
	period := 24 * time.Hour
	tick := time.Hour

	keys := make(map[string]struct{}, 1000)
	for range 1000 {
		keys[fmt.Sprintf("key-%d", rand.Int63())] = struct{}{}
	}

	periodStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Truncate(period)
	ticks := int(period / tick)
	tickSeconds := int64(tick / time.Second)

	seen := make(map[string]int)
	for i := range ticks {
		now := periodStart.Add(time.Duration(i)*tick + 15*time.Minute)
		fake := &fakeWQ{}
		sh, err := resync.New(tick, fake, resync.WithNow(staticNow(now)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := sh.Process(t.Context(), period, keys); err != nil {
			t.Fatalf("Process: %v", err)
		}
		for k, delay := range fake.enqueued {
			seen[k]++
			if delay < 0 || delay >= tickSeconds {
				t.Errorf("tick %d key %q: delay %ds, want in [0, %d)", i, k, delay, tickSeconds)
			}
		}
	}

	missing, extra := 0, 0
	for k := range keys {
		switch seen[k] {
		case 1:
		case 0:
			missing++
		default:
			extra++
		}
	}
	if missing != 0 || extra != 0 {
		t.Errorf("coverage: missing=%d extra=%d (want 0/0 across %d keys)", missing, extra, len(keys))
	}
}

// TestProcessStability asserts that two start values within the same tick
// produce identical enqueue results.
func TestProcessStability(t *testing.T) {
	period := 24 * time.Hour
	tick := time.Hour
	periodStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Truncate(period)

	keys := make(map[string]struct{}, 200)
	for i := range 200 {
		keys[fmt.Sprintf("k-%d", i)] = struct{}{}
	}

	t1 := periodStart.Add(2*tick + 5*time.Minute)
	t2 := periodStart.Add(2*tick + 45*time.Minute)

	f1 := &fakeWQ{}
	sh1, err := resync.New(tick, f1, resync.WithNow(staticNow(t1)))
	if err != nil {
		t.Fatalf("New(t1): %v", err)
	}
	if err := sh1.Process(t.Context(), period, keys); err != nil {
		t.Fatalf("Process(t1): %v", err)
	}

	f2 := &fakeWQ{}
	sh2, err := resync.New(tick, f2, resync.WithNow(staticNow(t2)))
	if err != nil {
		t.Fatalf("New(t2): %v", err)
	}
	if err := sh2.Process(t.Context(), period, keys); err != nil {
		t.Fatalf("Process(t2): %v", err)
	}

	if !reflect.DeepEqual(f1.enqueued, f2.enqueued) {
		t.Errorf("stability: f1=%v, f2=%v", f1.enqueued, f2.enqueued)
	}
}

// TestProcessReshuffle asserts that the set of keys assigned to a given
// shard differs substantially between consecutive periods.
func TestProcessReshuffle(t *testing.T) {
	period := 24 * time.Hour
	tick := time.Hour
	periodStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Truncate(period)

	keys := make(map[string]struct{}, 5000)
	for i := range 5000 {
		keys[fmt.Sprintf("k-%d", i)] = struct{}{}
	}

	f1 := &fakeWQ{}
	sh1, err := resync.New(tick, f1, resync.WithNow(staticNow(periodStart.Add(time.Minute))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sh1.Process(t.Context(), period, keys); err != nil {
		t.Fatalf("Process: %v", err)
	}

	f2 := &fakeWQ{}
	sh2, err := resync.New(tick, f2, resync.WithNow(staticNow(periodStart.Add(period+time.Minute))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sh2.Process(t.Context(), period, keys); err != nil {
		t.Fatalf("Process: %v", err)
	}

	overlap := 0
	for k := range f1.enqueued {
		if _, ok := f2.enqueued[k]; ok {
			overlap++
		}
	}
	// With independent reshuffle and 24 ticks per period, overlap is expected
	// to be ~|f1.enqueued| / 24 ≈ 4%. Allow up to 25% for variance.
	upper := len(f1.enqueued) / 4
	if overlap > upper {
		t.Errorf("reshuffle: overlap %d/%d, expected ~1/24 of shard size", overlap, len(f1.enqueued))
	}
}

// TestProcessErrorPropagation asserts that a workqueue error surfaces from
// Process. We use period == tick so every key falls in the single shard.
func TestProcessErrorPropagation(t *testing.T) {
	want := errors.New("boom")
	fake := &fakeWQ{processErr: want}
	keys := map[string]struct{}{"a": {}, "b": {}, "c": {}}

	sh, err := resync.New(time.Hour, fake, resync.WithNow(staticNow(time.Unix(1700000000, 0))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = sh.Process(t.Context(), time.Hour, keys)
	if err == nil {
		t.Fatal("Process: got = nil, want error")
	}
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Process error: got = %v, want wrapping %v", err, want)
	}
}

// TestProcessEmpty asserts that an empty keyset is handled cleanly.
func TestProcessEmpty(t *testing.T) {
	fake := &fakeWQ{}
	sh, err := resync.New(time.Hour, fake, resync.WithNow(staticNow(time.Unix(1700000000, 0))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sh.Process(t.Context(), 24*time.Hour, nil); err != nil {
		t.Errorf("Process(nil): got = %v, want nil", err)
	}
	if err := sh.Process(t.Context(), 24*time.Hour, map[string]struct{}{}); err != nil {
		t.Errorf("Process(empty): got = %v, want nil", err)
	}
	if len(fake.enqueued) != 0 {
		t.Errorf("enqueued: got = %d, want 0", len(fake.enqueued))
	}
}

// TestProcessLatencyCompensation asserts that the DelaySeconds passed to the
// workqueue is reduced by however much wall-clock time has elapsed between
// New and the moment of enqueue. Keys whose target instant has already passed
// fire immediately (DelaySeconds == 0).
func TestProcessLatencyCompensation(t *testing.T) {
	period := 24 * time.Hour
	tick := time.Hour
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	keys := make(map[string]struct{}, 500)
	for i := range 500 {
		keys[fmt.Sprintf("k-%d", i)] = struct{}{}
	}

	// Reference run: clock never advances, so DelaySeconds equals the key's
	// logical offset within the shard window.
	refFake := &fakeWQ{}
	refSh, err := resync.New(tick, refFake, resync.WithNow(staticNow(base)))
	if err != nil {
		t.Fatalf("New(ref): %v", err)
	}
	if err := refSh.Process(t.Context(), period, keys); err != nil {
		t.Fatalf("Process(ref): %v", err)
	}

	t.Run("clock advances by half a tick: delays shrink by that amount", func(t *testing.T) {
		clock := &fakeClock{t: base}
		fake := &fakeWQ{}
		sh, err := resync.New(tick, fake, resync.WithNow(clock.Now))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		const advance = 30 * time.Minute
		clock.Advance(advance)
		if err := sh.Process(t.Context(), period, keys); err != nil {
			t.Fatalf("Process: %v", err)
		}

		if len(fake.enqueued) != len(refFake.enqueued) {
			t.Fatalf("shard membership: got = %d keys, want = %d", len(fake.enqueued), len(refFake.enqueued))
		}
		advanceSec := int64(advance / time.Second)
		for k, logical := range refFake.enqueued {
			want := max(0, logical-advanceSec)
			if got := fake.enqueued[k]; got != want {
				t.Errorf("key %q: delay got = %d, want = %d (logical = %d, advance = %d)", k, got, want, logical, advanceSec)
			}
		}
	})

	t.Run("clock advances past tick end: delays clamp to zero", func(t *testing.T) {
		clock := &fakeClock{t: base}
		fake := &fakeWQ{}
		sh, err := resync.New(tick, fake, resync.WithNow(clock.Now))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		clock.Advance(2 * tick)
		if err := sh.Process(t.Context(), period, keys); err != nil {
			t.Fatalf("Process: %v", err)
		}
		for k, got := range fake.enqueued {
			if got != 0 {
				t.Errorf("key %q: delay got = %d, want = 0 (target already passed)", k, got)
			}
		}
	})

	t.Run("clock has not advanced: delays match the unchanged reference", func(t *testing.T) {
		clock := &fakeClock{t: base}
		fake := &fakeWQ{}
		sh, err := resync.New(tick, fake, resync.WithNow(clock.Now))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := sh.Process(t.Context(), period, keys); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if !reflect.DeepEqual(fake.enqueued, refFake.enqueued) {
			t.Error("delays diverge from the static-clock reference run")
		}
	})
}
