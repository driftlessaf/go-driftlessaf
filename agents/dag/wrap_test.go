/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dag_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"chainguard.dev/driftlessaf/agents/dag"
	"chainguard.dev/driftlessaf/agents/executor/retry"
)

var errTransient = errors.New("transient")

func isTransient(err error) bool { return errors.Is(err, errTransient) }

// failing returns a runner that fails with err on its first n calls, then
// succeeds, counting every call.
func failing(calls *atomic.Int32, n int32, err error) dag.StepRunner {
	return func(context.Context, dag.Input) ([]byte, error) {
		if calls.Add(1) <= n {
			return nil, err
		}
		return []byte("ok"), nil
	}
}

func TestRetry(t *testing.T) {
	cfg := func(retries int, maxBackoff time.Duration) retry.RetryConfig {
		return retry.RetryConfig{MaxRetries: retries, BaseBackoff: time.Second, MaxBackoff: maxBackoff}
	}
	tests := []struct {
		name        string
		cfg         retry.RetryConfig
		failures    int32
		err         error
		wantCalls   int32
		wantElapsed time.Duration
		wantErr     error
	}{
		{name: "recovers from transient errors", cfg: cfg(5, time.Minute), failures: 2, err: errTransient, wantCalls: 3, wantElapsed: 3 * time.Second},
		{name: "stops at max retries", cfg: cfg(2, time.Minute), failures: 100, err: errTransient, wantCalls: 3, wantElapsed: 3 * time.Second, wantErr: errTransient},
		{name: "backoff is capped", cfg: cfg(3, 1500*time.Millisecond), failures: 100, err: errTransient, wantCalls: 4, wantElapsed: 4 * time.Second, wantErr: errTransient},
		{name: "zero retries runs once", cfg: cfg(0, time.Minute), failures: 100, err: errTransient, wantCalls: 1, wantErr: errTransient},
		{name: "permanent error is not retried", cfg: cfg(5, time.Minute), failures: 100, err: errors.New("permanent"), wantCalls: 1},
		{name: "deferral is not retried", cfg: cfg(5, time.Minute), failures: 100, err: fmt.Errorf("batch pending: %w (%w)", dag.ErrDeferred, errTransient), wantCalls: 1, wantErr: dag.ErrDeferred},
		{name: "invalid config runs nothing", cfg: cfg(-1, time.Minute), failures: 100, err: errTransient, wantCalls: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				run := dag.Retry(tc.cfg, "step", isTransient, failing(&calls, tc.failures, tc.err))
				start := time.Now()
				out, err := run(t.Context(), dag.Input{})
				if got := calls.Load(); got != tc.wantCalls {
					t.Errorf("calls: got = %d, want = %d", got, tc.wantCalls)
				}
				if got := time.Since(start); got != tc.wantElapsed {
					t.Errorf("elapsed: got = %v, want = %v", got, tc.wantElapsed)
				}
				switch {
				case tc.wantCalls > tc.failures:
					if err != nil || string(out) != "ok" {
						t.Errorf("run: got = (%q, %v), want = (\"ok\", nil)", out, err)
					}
				case err == nil:
					t.Error("run: got = nil error, want an error")
				case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
					t.Errorf("run: got = %v, want an error wrapping %v", err, tc.wantErr)
				}
			})
		})
	}
}

func TestRetryStopsOnCancel(t *testing.T) {
	cfg := retry.RetryConfig{MaxRetries: 5, BaseBackoff: time.Hour, MaxBackoff: time.Hour}
	t.Run("already cancelled", func(t *testing.T) {
		var calls atomic.Int32
		ctx, cancel := context.WithCancelCause(t.Context())
		stop := errors.New("operator stopped the run")
		cancel(stop)
		out, err := dag.Retry(cfg, "step", isTransient, failing(&calls, 0, nil))(ctx, dag.Input{})
		if out != nil || !errors.Is(err, stop) {
			t.Errorf("run: got = (%q, %v), want = (nil, the cancel cause)", out, err)
		}
		if got := calls.Load(); got != 0 {
			t.Errorf("calls: got = %d, want = 0", got)
		}
	})
	t.Run("cancelled by the runner", func(t *testing.T) {
		var calls atomic.Int32
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		run := dag.Retry(cfg, "step", isTransient, func(context.Context, dag.Input) ([]byte, error) {
			calls.Add(1)
			cancel()
			return nil, errTransient
		})
		if _, err := run(ctx, dag.Input{}); !errors.Is(err, errTransient) {
			t.Errorf("run: got = %v, want the runner's error", err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("calls: got = %d, want = 1", got)
		}
	})
	t.Run("cancelled between attempts with zero backoff", func(t *testing.T) {
		// The backoff select picks at random between a done ctx and a ready
		// timer, so one trial would pass by chance half the time.
		for range 50 {
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(t.Context())
			cfg := retry.RetryConfig{MaxRetries: 5, OnAttemptError: func(error) { cancel() }}
			_, err := dag.Retry(cfg, "step", isTransient, failing(&calls, 1, errTransient))(ctx, dag.Input{})
			cancel()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("run: got = %v, want context.Canceled", err)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("calls: got = %d, want = 1", got)
			}
		}
	})
	t.Run("cancelled during backoff", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var calls atomic.Int32
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			stop := errors.New("operator stopped the run")
			go func() {
				time.Sleep(time.Minute)
				cancel(stop)
			}()
			start := time.Now()
			_, err := dag.Retry(cfg, "step", isTransient, failing(&calls, 100, errTransient))(ctx, dag.Input{})
			if !errors.Is(err, stop) {
				t.Errorf("run: got = %v, want the cancel cause", err)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("calls: got = %d, want = 1", got)
			}
			if got := time.Since(start); got != time.Minute {
				t.Errorf("elapsed: got = %v, want = %v", got, time.Minute)
			}
		})
	})
}

func TestRetryNilPredicateRetriesNothing(t *testing.T) {
	var calls atomic.Int32
	cfg := retry.RetryConfig{MaxRetries: 5, BaseBackoff: time.Second, MaxBackoff: time.Minute}
	if _, err := dag.Retry(cfg, "step", nil, failing(&calls, 100, errTransient))(t.Context(), dag.Input{}); !errors.Is(err, errTransient) {
		t.Errorf("run: got = %v, want the runner's error", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls: got = %d, want = 1", got)
	}
}

func TestIterate(t *testing.T) {
	tests := []struct {
		name      string
		max       int
		doneAt    int // the pass whose output is done; 0 means never
		err       error
		wantPrevs []string
		wantOut   string
		wantErr   error
	}{
		{name: "done on the first pass", max: 5, doneAt: 1, wantPrevs: []string{""}, wantOut: "1"},
		{name: "done early", max: 5, doneAt: 3, wantPrevs: []string{"", "1", "12"}, wantOut: "123"},
		{name: "done on the last pass", max: 3, doneAt: 3, wantPrevs: []string{"", "1", "12"}, wantOut: "123"},
		{name: "cap returns the last output", max: 3, wantPrevs: []string{"", "1", "12"}, wantOut: "123", wantErr: dag.ErrMaxIterations},
		{name: "below one means one", max: 0, wantPrevs: []string{""}, wantOut: "1", wantErr: dag.ErrMaxIterations},
		{name: "error stops", max: 5, err: errTransient, wantPrevs: []string{""}, wantErr: errTransient},
		{name: "deferral stops", max: 5, err: dag.ErrDeferred, wantPrevs: []string{""}, wantErr: dag.ErrDeferred},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var prevs []string
			run := dag.Iterate(tc.max, func(_ context.Context, _ dag.Input, prev []byte) ([]byte, bool, error) {
				prevs = append(prevs, string(prev))
				if tc.err != nil {
					return nil, false, tc.err
				}
				// Each pass revises the previous one by appending its own number.
				out := strconv.AppendInt(bytes.Clone(prev), int64(len(prevs)), 10)
				return out, len(prevs) == tc.doneAt, nil
			})
			out, err := run(t.Context(), dag.Input{})
			if diff := cmp.Diff(tc.wantPrevs, prevs); diff != "" {
				t.Errorf("prev per pass (-want, +got):\n%s", diff)
			}
			if string(out) != tc.wantOut {
				t.Errorf("output: got = %q, want = %q", out, tc.wantOut)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("error: got = %v, want = %v", err, tc.wantErr)
			}
		})
	}
}

func TestIterateFirstPrevIsNil(t *testing.T) {
	var first []byte
	seen := false
	dag.Iterate(1, func(_ context.Context, _ dag.Input, prev []byte) ([]byte, bool, error) {
		first, seen = prev, true
		return nil, true, nil
	})(t.Context(), dag.Input{})
	if !seen || first != nil {
		t.Errorf("first prev: got = %#v (called %v), want = nil", first, seen)
	}
}

func TestRetryNilRunPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Retry(nil run): got no panic, want one at construction")
		}
	}()
	dag.Retry(retry.RetryConfig{}, "op", nil, nil)
}

func TestIterateNilStepPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Iterate(nil): got no panic, want one at construction")
		}
	}()
	dag.Iterate(3, nil)
}

func TestIterateStopsOnCancel(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	stop := errors.New("operator stopped the run")
	run := dag.Iterate(5, func(context.Context, dag.Input, []byte) ([]byte, bool, error) {
		calls.Add(1)
		cancel(stop)
		return []byte("draft"), false, nil
	})
	if _, err := run(ctx, dag.Input{}); !errors.Is(err, stop) {
		t.Errorf("error: got = %v, want the cancel cause", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls: got = %d, want = 1", got)
	}
}

// Wrapped runners must stay race-free and keep each step's outcome when the
// scheduler runs them side by side.
func TestWrappersUnderScheduler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := retry.RetryConfig{MaxRetries: 3, BaseBackoff: time.Second, MaxBackoff: time.Minute}
		var flakyCalls, deferredCalls, refineCalls, cappedCalls atomic.Int32
		// counter's pass n outputs n and is done on the third pass.
		counter := func(calls *atomic.Int32) func(context.Context, dag.Input, []byte) ([]byte, bool, error) {
			return func(context.Context, dag.Input, []byte) ([]byte, bool, error) {
				n := calls.Add(1)
				return []byte(strconv.Itoa(int(n))), n == 3, nil
			}
		}
		g, err := dag.New([]dag.Step{
			{Name: "flaky", Run: dag.Retry(cfg, "flaky", isTransient, failing(&flakyCalls, 2, errTransient))},
			{Name: "deferred", Run: dag.Retry(cfg, "deferred", func(error) bool { return true }, failing(&deferredCalls, 100, dag.ErrDeferred))},
			{Name: "refine", Run: dag.Retry(cfg, "refine", isTransient, dag.Iterate(5, counter(&refineCalls)))},
			{Name: "capped", Run: dag.Iterate(2, counter(&cappedCalls))},
			{Name: "merge", Deps: []string{"flaky", "refine"}, OptionalDeps: []string{"capped"}, Run: func(_ context.Context, in dag.Input) ([]byte, error) {
				return fmt.Appendf(nil, "%s+%s", in.Reads["flaky"], in.Reads["refine"]), nil
			}},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		res := g.Run(t.Context(), dag.WithMaxConcurrency(4))

		want := map[string]dag.StepResult{
			"flaky":    {Status: dag.StatusSucceeded, Output: []byte("ok")},
			"deferred": {Status: dag.StatusDeferred, Err: dag.ErrDeferred},
			"refine":   {Status: dag.StatusSucceeded, Output: []byte("3")},
			"capped":   {Status: dag.StatusFailed, Err: dag.ErrMaxIterations},
			"merge":    {Status: dag.StatusSucceeded, Output: []byte("ok+3")},
		}
		errorsMatch := cmp.Comparer(func(x, y error) bool { return errors.Is(x, y) || errors.Is(y, x) })
		if diff := cmp.Diff(want, res.Steps, errorsMatch); diff != "" {
			t.Errorf("results (-want, +got):\n%s", diff)
		}
		for name, c := range map[string]struct {
			calls *atomic.Int32
			want  int32
		}{"flaky": {&flakyCalls, 3}, "deferred": {&deferredCalls, 1}, "refine": {&refineCalls, 3}, "capped": {&cappedCalls, 2}} {
			if got := c.calls.Load(); got != c.want {
				t.Errorf("%s calls: got = %d, want = %d", name, got, c.want)
			}
		}
	})
}

// Each wrapper must hand every attempt or pass the Input it got: a dependent
// needs its upstream outputs, and a deferring runner the hash to store under.
func TestWrappersForwardInput(t *testing.T) {
	// Each wrap needs three calls to succeed: Retry sees two transient
	// failures, Iterate two passes that are not done.
	wrappers := []struct {
		name string
		wrap func(record func(dag.Input) int) dag.StepRunner
	}{{
		name: "retry",
		wrap: func(record func(dag.Input) int) dag.StepRunner {
			cfg := retry.RetryConfig{MaxRetries: 5, BaseBackoff: time.Second, MaxBackoff: time.Minute}
			return dag.Retry(cfg, "step", isTransient, func(_ context.Context, in dag.Input) ([]byte, error) {
				if record(in) < 3 {
					return nil, errTransient
				}
				return []byte("ok"), nil
			})
		},
	}, {
		name: "iterate",
		wrap: func(record func(dag.Input) int) dag.StepRunner {
			return dag.Iterate(5, func(_ context.Context, in dag.Input, _ []byte) ([]byte, bool, error) {
				return []byte("ok"), record(in) == 3, nil
			})
		},
	}}
	for _, w := range wrappers {
		t.Run(w.name+" called directly", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var got []dag.Input
				in := dag.Input{
					Reads: map[string][]byte{"plan": []byte(strconv.FormatUint(rand.Uint64(), 36))},
					Hash:  strconv.FormatUint(rand.Uint64(), 16),
				}
				out, err := w.wrap(func(in dag.Input) int {
					got = append(got, in)
					return len(got)
				})(t.Context(), in)
				if err != nil || string(out) != "ok" {
					t.Fatalf("run: got = (%q, %v), want = (\"ok\", nil)", out, err)
				}
				if diff := cmp.Diff([]dag.Input{in, in, in}, got); diff != "" {
					t.Errorf("input per call (-want, +got):\n%s", diff)
				}
			})
		})
		t.Run(w.name+" as a dependent under the scheduler", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var got []dag.Input
				draft := []byte(strconv.FormatUint(rand.Uint64(), 36))
				g, err := dag.New([]dag.Step{
					{Name: "root", Run: func(context.Context, dag.Input) ([]byte, error) { return draft, nil }},
					{
						Name: "wrapped",
						Deps: []string{"root"},
						Hash: func(_ context.Context, in dag.Input) (string, error) {
							return "sha:" + string(in.Reads["root"]), nil
						},
						Run: w.wrap(func(in dag.Input) int {
							got = append(got, in)
							return len(got)
						}),
					},
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if r := g.Run(t.Context()).Steps["wrapped"]; r.Status != dag.StatusSucceeded {
					t.Fatalf("wrapped: got = %+v, want StatusSucceeded", r)
				}
				want := dag.Input{Reads: map[string][]byte{"root": draft}, Hash: "sha:" + string(draft)}
				if diff := cmp.Diff([]dag.Input{want, want, want}, got); diff != "" {
					t.Errorf("input per call (-want, +got):\n%s", diff)
				}
			})
		})
	}
}

// A panic is not an error to retry: under Graph.Run it fails the step on the
// first attempt or pass, even with a predicate that retries everything.
func TestWrappersPanicFailsStep(t *testing.T) {
	cfg := retry.RetryConfig{MaxRetries: 3, BaseBackoff: time.Second, MaxBackoff: time.Minute}
	tests := []struct {
		name string
		wrap func(calls *atomic.Int32, value string) dag.StepRunner
	}{{
		name: "retry",
		wrap: func(calls *atomic.Int32, value string) dag.StepRunner {
			return dag.Retry(cfg, "step", func(error) bool { return true }, func(context.Context, dag.Input) ([]byte, error) {
				calls.Add(1)
				panic(value)
			})
		},
	}, {
		name: "iterate",
		wrap: func(calls *atomic.Int32, value string) dag.StepRunner {
			return dag.Iterate(3, func(context.Context, dag.Input, []byte) ([]byte, bool, error) {
				calls.Add(1)
				panic(value)
			})
		},
	}, {
		name: "retry around iterate",
		wrap: func(calls *atomic.Int32, value string) dag.StepRunner {
			return dag.Retry(cfg, "step", func(error) bool { return true }, dag.Iterate(3, func(context.Context, dag.Input, []byte) ([]byte, bool, error) {
				calls.Add(1)
				panic(value)
			}))
		},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				value := strconv.FormatUint(rand.Uint64(), 36)
				g, err := dag.New([]dag.Step{{Name: "a", Run: tc.wrap(&calls, value)}})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				got := g.Run(t.Context()).Steps["a"]
				if got.Status != dag.StatusFailed || got.Err == nil || !strings.Contains(got.Err.Error(), value) {
					t.Errorf("a: got = %v (%v), want StatusFailed with an Err containing %q", got.Status, got.Err, value)
				}
				if n := calls.Load(); n != 1 {
					t.Errorf("calls: got = %d, want = 1", n)
				}
			})
		})
	}
}
