/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dag_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"chainguard.dev/driftlessaf/agents/dag"
	"chainguard.dev/driftlessaf/agents/dag/dagtest"
	"chainguard.dev/driftlessaf/agents/dag/internal/schedtest"
)

func TestConformance(t *testing.T) {
	schedtest.RunConformance(t, func(ctx context.Context, steps []dag.Step, cfg schedtest.Config) (dag.Result, error) {
		g, err := dag.New(steps)
		if err != nil {
			return dag.Result{}, err
		}
		opts := []dag.Option{dag.WithResumer(cfg.Resumer)}
		if cfg.MaxConcurrency > 0 {
			opts = append(opts, dag.WithMaxConcurrency(cfg.MaxConcurrency))
		}
		return g.Run(ctx, opts...), nil
	})
}

// A sibling's cancel can land while a step is finishing; the step's paid
// output must still be saved. Serial can't run siblings, so this lives here.
func TestSaveAfterSiblingCancel(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New(rand.Text())
	paid := []byte(rand.Text())
	store := dagtest.NewMem()
	g, err := dag.New([]dag.Step{
		{Name: "paid", Hash: func(context.Context, dag.Input) (string, error) { return "h", nil }, Run: func(ctx context.Context, _ dag.Input) ([]byte, error) {
			<-ctx.Done()
			return paid, nil
		}},
		{Name: "stopper", Run: func(context.Context, dag.Input) ([]byte, error) {
			cancel(cause)
			return nil, cause
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res := g.Run(ctx, dag.WithResumer(schedtest.CtxMem{Mem: store}))
	if got := res.Steps["paid"]; got.Status != dag.StatusSucceeded || !bytes.Equal(got.Output, paid) {
		t.Errorf("paid: got = %v %q (err %v), want = %v %q", got.Status, got.Output, got.Err, dag.StatusSucceeded, paid)
	}
	if got, found, _ := store.Lookup(t.Context(), "paid", "h"); !found || !bytes.Equal(got, paid) {
		t.Errorf("stored paid output: got = %q (found %v), want = %q", got, found, paid)
	}
}

// The suite pins that a panic fails its step; this pins that Run's Err
// carries the stack, which Serial's does not.
func TestPanicErrCarriesStack(t *testing.T) {
	value := rand.Text()
	g, err := dag.New([]dag.Step{{Name: "a", Run: func(context.Context, dag.Input) ([]byte, error) { panic(value) }}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := g.Run(t.Context()).Steps["a"].Err
	for _, want := range []string{`step "a" panicked: ` + value, "goroutine ", "TestPanicErrCarriesStack"} {
		if got == nil || !strings.Contains(got.Error(), want) {
			t.Errorf("a.Err: got = %v, want it to contain %q", got, want)
		}
	}
}

// The suite only bounds concurrency from above; this pins that Run fills
// every slot rather than running ready steps one at a time.
func TestRunFillsConcurrencySlots(t *testing.T) {
	tests := []struct {
		name  string
		opts  []dag.Option
		width int
		want  int
	}{
		{name: "capped", opts: []dag.Option{dag.WithMaxConcurrency(3)}, width: 7, want: 3},
		{name: "default is four", width: 7, want: 4},
		{name: "below one means one", opts: []dag.Option{dag.WithMaxConcurrency(0)}, width: 4, want: 1},
		{name: "cap above width", opts: []dag.Option{dag.WithMaxConcurrency(10)}, width: 5, want: 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var (
					mu       sync.Mutex
					inflight int
				)
				release := make(chan struct{})
				steps := make([]dag.Step, 0, tc.width)
				for i := range tc.width {
					steps = append(steps, dag.Step{Name: fmt.Sprint(i), Run: func(context.Context, dag.Input) ([]byte, error) {
						mu.Lock()
						inflight++
						mu.Unlock()
						<-release
						mu.Lock()
						inflight--
						mu.Unlock()
						return nil, nil
					}})
				}
				g, err := dag.New(steps)
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				var res dag.Result
				done := make(chan struct{})
				go func() {
					defer close(done)
					res = g.Run(t.Context(), tc.opts...)
				}()

				synctest.Wait()
				mu.Lock()
				got := inflight
				mu.Unlock()
				if got != tc.want {
					t.Errorf("in flight: got = %d, want = %d", got, tc.want)
				}
				close(release)
				<-done
				for name, r := range res.Steps {
					if r.Status != dag.StatusSucceeded {
						t.Errorf("%s: got = %v, want = %v", name, r.Status, dag.StatusSucceeded)
					}
				}
			})
		})
	}
}

func TestStatusString(t *testing.T) {
	tests := []struct {
		status dag.Status
		want   string
	}{
		{dag.StatusSucceeded, "succeeded"},
		{dag.StatusFailed, "failed"},
		{dag.StatusSkipped, "skipped"},
		{dag.StatusDeferred, "deferred"},
		{dag.StatusWaiting, "waiting"},
		{dag.Status(0), "Status(0)"},
	}
	for _, tc := range tests {
		if got := tc.status.String(); got != tc.want {
			t.Errorf("Status(%d).String(): got = %q, want = %q", int(tc.status), got, tc.want)
		}
	}
}
