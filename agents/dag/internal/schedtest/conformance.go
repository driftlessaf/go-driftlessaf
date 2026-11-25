/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package schedtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/dag"
	"chainguard.dev/driftlessaf/agents/dag/dagtest"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// RunFunc validates and runs steps under cfg, as a scheduler under test does.
type RunFunc func(ctx context.Context, steps []dag.Step, cfg Config) (dag.Result, error)

// recorder supplies runners that return a fixed outcome and log each call.
type recorder struct {
	mu     sync.Mutex
	calls  map[string]int
	inputs map[string]dag.Input
}

func newRecorder() *recorder {
	return &recorder{calls: map[string]int{}, inputs: map[string]dag.Input{}}
}

func (r *recorder) runner(name string, out []byte, err error) dag.StepRunner {
	return func(_ context.Context, in dag.Input) ([]byte, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls[name]++
		r.inputs[name] = dag.Input{Reads: maps.Clone(in.Reads), Hash: in.Hash}
		return out, err
	}
}

func (r *recorder) called(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[name]
}

// hashOf is a caller-side input hash over a fingerprint and the reads.
func hashOf(fingerprint string) func(context.Context, dag.Input) (string, error) {
	return func(_ context.Context, in dag.Input) (string, error) {
		h := sha256.New()
		h.Write([]byte(fingerprint))
		for _, k := range slices.Sorted(maps.Keys(in.Reads)) {
			h.Write([]byte{0})
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write(in.Reads[k])
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
}

// failingResumer fails Lookup or Save with a fixed error.
type failingResumer struct {
	lookupErr, saveErr error
}

func (f failingResumer) Lookup(context.Context, string, string) ([]byte, bool, error) {
	return nil, false, f.lookupErr
}

func (f failingResumer) Save(context.Context, string, string, []byte) error { return f.saveErr }

// CtxMem is a dagtest.Mem whose Save fails once its ctx is done, as a
// networked store does.
type CtxMem struct{ *dagtest.Mem }

func (c CtxMem) Save(ctx context.Context, step, hash string, output []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.Mem.Save(ctx, step, hash, output)
}

// panickingResumer panics in Lookup or Save for one step and defers to Mem
// for every other call.
type panickingResumer struct {
	*dagtest.Mem
	step         string
	lookup, save bool
	value        string
}

func (p panickingResumer) Lookup(ctx context.Context, step, hash string) ([]byte, bool, error) {
	if p.lookup && step == p.step {
		panic(p.value)
	}
	return p.Mem.Lookup(ctx, step, hash)
}

func (p panickingResumer) Save(ctx context.Context, step, hash string, output []byte) error {
	if p.save && step == p.step {
		panic(p.value)
	}
	return p.Mem.Save(ctx, step, hash, output)
}

func randomBytes() []byte { return []byte(rand.Text()) }

func statuses(res dag.Result) map[string]dag.Status {
	out := make(map[string]dag.Status, len(res.Steps))
	for k, v := range res.Steps {
		out[k] = v.Status
	}
	return out
}

// errorsMatch compares errors by errors.Is in either direction.
var errorsMatch = cmp.Comparer(func(x, y error) bool { return errors.Is(x, y) || errors.Is(y, x) })

func mustRun(t *testing.T, run RunFunc, steps []dag.Step, cfg Config) dag.Result {
	t.Helper()
	res, err := run(t.Context(), steps, cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

// RunConformance holds run to the dag scheduling contract.
func RunConformance(t *testing.T, run RunFunc) {
	t.Run("rejects invalid graphs", func(t *testing.T) { testRejects(t, run) })
	t.Run("passes outputs along edges", func(t *testing.T) { testOutputs(t, run) })
	t.Run("failure skips required dependents only", func(t *testing.T) { testFailure(t, run) })
	t.Run("deferral leaves dependents waiting", func(t *testing.T) { testDeferral(t, run) })
	t.Run("resume", func(t *testing.T) { testResume(t, run) })
	t.Run("a panicking callback fails its step only", func(t *testing.T) { testPanic(t, run) })
	t.Run("cancellation skips unstarted steps", func(t *testing.T) { testCancel(t, run) })
	t.Run("respects max concurrency", func(t *testing.T) { testConcurrency(t, run) })
}

func testRejects(t *testing.T, run RunFunc) {
	tests := []struct {
		name    string
		steps   func(rec *recorder) []dag.Step
		mention string
	}{{
		name: "empty name",
		steps: func(rec *recorder) []dag.Step {
			return []dag.Step{{Name: "", Run: rec.runner("", nil, nil)}}
		},
		mention: "no name",
	}, {
		name: "duplicate name",
		steps: func(rec *recorder) []dag.Step {
			return []dag.Step{{Name: "a", Run: rec.runner("a", nil, nil)}, {Name: "a", Run: rec.runner("a", nil, nil)}}
		},
		mention: `"a"`,
	}, {
		name: "nil runner",
		steps: func(*recorder) []dag.Step {
			return []dag.Step{{Name: "a"}}
		},
		mention: `"a"`,
	}, {
		name: "dangling dependency",
		steps: func(rec *recorder) []dag.Step {
			return []dag.Step{{Name: "a", Deps: []string{"ghost"}, Run: rec.runner("a", nil, nil)}}
		},
		mention: `"ghost"`,
	}, {
		name: "dangling optional dependency",
		steps: func(rec *recorder) []dag.Step {
			return []dag.Step{{Name: "a", OptionalDeps: []string{"ghost"}, Run: rec.runner("a", nil, nil)}}
		},
		mention: `"ghost"`,
	}, {
		name: "dependency listed twice",
		steps: func(rec *recorder) []dag.Step {
			return []dag.Step{
				{Name: "a", Run: rec.runner("a", nil, nil)},
				{Name: "b", Deps: []string{"a"}, OptionalDeps: []string{"a"}, Run: rec.runner("b", nil, nil)},
			}
		},
		mention: `"a"`,
	}, {
		name: "self loop",
		steps: func(rec *recorder) []dag.Step {
			return []dag.Step{{Name: "a", Deps: []string{"a"}, Run: rec.runner("a", nil, nil)}}
		},
		mention: "a -> a",
	}, {
		name: "cycle through an optional edge",
		steps: func(rec *recorder) []dag.Step {
			return []dag.Step{
				{Name: "root", Run: rec.runner("root", nil, nil)},
				{Name: "a", Deps: []string{"root", "c"}, Run: rec.runner("a", nil, nil)},
				{Name: "b", Deps: []string{"a"}, Run: rec.runner("b", nil, nil)},
				{Name: "c", OptionalDeps: []string{"b"}, Run: rec.runner("c", nil, nil)},
			}
		},
		mention: "a -> c -> b -> a",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder()
			_, err := run(t.Context(), tc.steps(rec), Config{})
			if err == nil {
				t.Fatal("run: got nil error, want a rejection")
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error: got = %q, want it to mention %q", err, tc.mention)
			}
			if len(rec.calls) != 0 {
				t.Errorf("runners called: got = %v, want none", rec.calls)
			}
		})
	}
}

func testOutputs(t *testing.T, run RunFunc) {
	rec := newRecorder()
	a, b, c, d := randomBytes(), randomBytes(), randomBytes(), randomBytes()
	// A diamond declared out of dependency order.
	res := mustRun(t, run, []dag.Step{
		{Name: "d", Deps: []string{"b", "c"}, Run: rec.runner("d", d, nil)},
		{Name: "b", Deps: []string{"a"}, Run: rec.runner("b", b, nil)},
		{Name: "c", OptionalDeps: []string{"a"}, Run: rec.runner("c", c, nil)},
		{Name: "a", Run: rec.runner("a", a, nil)},
	}, Config{})

	want := map[string]dag.StepResult{
		"a": {Status: dag.StatusSucceeded, Output: a},
		"b": {Status: dag.StatusSucceeded, Output: b},
		"c": {Status: dag.StatusSucceeded, Output: c},
		"d": {Status: dag.StatusSucceeded, Output: d},
	}
	if diff := cmp.Diff(want, res.Steps); diff != "" {
		t.Errorf("results (-want, +got):\n%s", diff)
	}
	wantInputs := map[string]dag.Input{
		"a": {},
		"b": {Reads: map[string][]byte{"a": a}},
		"c": {Reads: map[string][]byte{"a": a}},
		"d": {Reads: map[string][]byte{"b": b, "c": c}},
	}
	if diff := cmp.Diff(wantInputs, rec.inputs, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("inputs (-want, +got):\n%s", diff)
	}
}

func testFailure(t *testing.T, run RunFunc) {
	rec := newRecorder()
	boom := errors.New(rand.Text())
	ok := randomBytes()
	res := mustRun(t, run, []dag.Step{
		{Name: "bad", Run: rec.runner("bad", nil, boom)},
		{Name: "good", Run: rec.runner("good", ok, nil)},
		{Name: "needs-bad", Deps: []string{"bad"}, Run: rec.runner("needs-bad", nil, nil)},
		{Name: "after-skip", Deps: []string{"needs-bad"}, Run: rec.runner("after-skip", nil, nil)},
		{Name: "tolerates-bad", Deps: []string{"good"}, OptionalDeps: []string{"bad"}, Run: rec.runner("tolerates-bad", nil, nil)},
		{Name: "tolerates-skip", OptionalDeps: []string{"needs-bad"}, Run: rec.runner("tolerates-skip", nil, nil)},
	}, Config{})

	want := map[string]dag.Status{
		"bad":            dag.StatusFailed,
		"good":           dag.StatusSucceeded,
		"needs-bad":      dag.StatusSkipped,
		"after-skip":     dag.StatusSkipped,
		"tolerates-bad":  dag.StatusSucceeded,
		"tolerates-skip": dag.StatusSucceeded,
	}
	if diff := cmp.Diff(want, statuses(res)); diff != "" {
		t.Errorf("statuses (-want, +got):\n%s", diff)
	}
	if got := res.Steps["bad"].Err; !errors.Is(got, boom) {
		t.Errorf("bad.Err: got = %v, want %v", got, boom)
	}
	for _, name := range []string{"needs-bad", "after-skip"} {
		if n := rec.called(name); n != 0 {
			t.Errorf("%s runner calls: got = %d, want = 0", name, n)
		}
	}
	if diff := cmp.Diff(map[string][]byte{"good": ok}, rec.inputs["tolerates-bad"].Reads); diff != "" {
		t.Errorf("tolerates-bad reads (-want, +got):\n%s", diff)
	}
}

func testDeferral(t *testing.T, run RunFunc) {
	rec := newRecorder()
	deferred := errors.Join(dag.ErrDeferred, errors.New(rand.Text()))
	res := mustRun(t, run, []dag.Step{
		{Name: "batch", Run: rec.runner("batch", nil, deferred)},
		{Name: "bad", Run: rec.runner("bad", nil, errors.New("boom"))},
		{Name: "other", Run: rec.runner("other", randomBytes(), nil)},
		{Name: "needs-batch", Deps: []string{"batch"}, Run: rec.runner("needs-batch", nil, nil)},
		{Name: "gathers-batch", OptionalDeps: []string{"batch", "other"}, Run: rec.runner("gathers-batch", nil, nil)},
		{Name: "after-wait", Deps: []string{"needs-batch"}, Run: rec.runner("after-wait", nil, nil)},
		{Name: "broken-and-waiting", Deps: []string{"bad", "batch"}, Run: rec.runner("broken-and-waiting", nil, nil)},
		{Name: "waiting-and-broken", Deps: []string{"batch", "bad"}, Run: rec.runner("waiting-and-broken", nil, nil)},
		{Name: "after-other", Deps: []string{"other"}, Run: rec.runner("after-other", nil, nil)},
	}, Config{})

	want := map[string]dag.Status{
		"batch":              dag.StatusDeferred,
		"bad":                dag.StatusFailed,
		"other":              dag.StatusSucceeded,
		"needs-batch":        dag.StatusWaiting,
		"gathers-batch":      dag.StatusWaiting,
		"after-wait":         dag.StatusWaiting,
		"broken-and-waiting": dag.StatusSkipped,
		"waiting-and-broken": dag.StatusSkipped,
		"after-other":        dag.StatusSucceeded,
	}
	if diff := cmp.Diff(want, statuses(res)); diff != "" {
		t.Errorf("statuses (-want, +got):\n%s", diff)
	}
	for _, name := range []string{"batch", "needs-batch", "gathers-batch", "after-wait"} {
		if err := res.Steps[name].Err; !errors.Is(err, dag.ErrDeferred) {
			t.Errorf("%s.Err: got = %v, want it to wrap ErrDeferred", name, err)
		}
	}
	for _, name := range []string{"needs-batch", "gathers-batch", "after-wait", "broken-and-waiting", "waiting-and-broken"} {
		if n := rec.called(name); n != 0 {
			t.Errorf("%s runner calls: got = %d, want = 0", name, n)
		}
	}
}

func testResume(t *testing.T, run RunFunc) {
	mustNotRun := errors.New("must not run")

	t.Run("a second run serves every hashed step from the store", func(t *testing.T) {
		store := dagtest.NewMem()
		a, b, script := randomBytes(), randomBytes(), randomBytes()
		first := newRecorder()
		mustRun(t, run, []dag.Step{
			{Name: "a", Hash: hashOf("a"), Run: first.runner("a", a, nil)},
			{Name: "b", Deps: []string{"a"}, Hash: hashOf("b"), Run: first.runner("b", b, nil)},
			{Name: "script", Run: first.runner("script", script, nil)},
		}, Config{Resumer: store})

		second := newRecorder()
		res := mustRun(t, run, []dag.Step{
			{Name: "a", Hash: hashOf("a"), Run: second.runner("a", nil, mustNotRun)},
			{Name: "b", Deps: []string{"a"}, Hash: hashOf("b"), Run: second.runner("b", nil, mustNotRun)},
			{Name: "script", Run: second.runner("script", script, nil)},
		}, Config{Resumer: store})
		want := map[string]dag.StepResult{
			"a":      {Status: dag.StatusSucceeded, Output: a, Resumed: true},
			"b":      {Status: dag.StatusSucceeded, Output: b, Resumed: true},
			"script": {Status: dag.StatusSucceeded, Output: script},
		}
		if diff := cmp.Diff(want, res.Steps); diff != "" {
			t.Errorf("results (-want, +got):\n%s", diff)
		}
		if diff := cmp.Diff(map[string]int{"script": 1}, second.calls); diff != "" {
			t.Errorf("runner calls (-want, +got):\n%s", diff)
		}
	})

	t.Run("a changed hash re-runs the step and its dependents", func(t *testing.T) {
		store := dagtest.NewMem()
		first := newRecorder()
		mustRun(t, run, []dag.Step{
			{Name: "a", Hash: hashOf("v1"), Run: first.runner("a", randomBytes(), nil)},
			{Name: "b", Deps: []string{"a"}, Hash: hashOf("b"), Run: first.runner("b", randomBytes(), nil)},
			{Name: "c", Hash: hashOf("c"), Run: first.runner("c", randomBytes(), nil)},
		}, Config{Resumer: store})

		second := newRecorder()
		a2, b2 := randomBytes(), randomBytes()
		res := mustRun(t, run, []dag.Step{
			{Name: "a", Hash: hashOf("v2"), Run: second.runner("a", a2, nil)},
			{Name: "b", Deps: []string{"a"}, Hash: hashOf("b"), Run: second.runner("b", b2, nil)},
			{Name: "c", Hash: hashOf("c"), Run: second.runner("c", nil, mustNotRun)},
		}, Config{Resumer: store})
		if diff := cmp.Diff(map[string]int{"a": 1, "b": 1}, second.calls); diff != "" {
			t.Errorf("runner calls (-want, +got):\n%s", diff)
		}
		if diff := cmp.Diff(map[string][]byte{"a": a2}, second.inputs["b"].Reads); diff != "" {
			t.Errorf("b reads (-want, +got):\n%s", diff)
		}
		if !res.Steps["c"].Resumed {
			t.Error("c: got Resumed = false, want true")
		}
	})

	t.Run("a later dispatch picks up a deferred step under the hash its runner saw", func(t *testing.T) {
		store := dagtest.NewMem()
		a, b, c := randomBytes(), randomBytes(), randomBytes()
		first := newRecorder()
		res := mustRun(t, run, []dag.Step{
			{Name: "a", Hash: hashOf("a"), Run: first.runner("a", a, nil)},
			{Name: "b", Deps: []string{"a"}, Hash: hashOf("b"), Run: first.runner("b", nil, dag.ErrDeferred)},
			{Name: "c", Deps: []string{"b"}, Hash: hashOf("c"), Run: first.runner("c", c, nil)},
		}, Config{Resumer: store})
		want := map[string]dag.Status{"a": dag.StatusSucceeded, "b": dag.StatusDeferred, "c": dag.StatusWaiting}
		if diff := cmp.Diff(want, statuses(res)); diff != "" {
			t.Errorf("statuses after first run (-want, +got):\n%s", diff)
		}
		if _, found, _ := store.Lookup(t.Context(), "b", first.inputs["b"].Hash); found {
			t.Error("deferred step b was saved, want it left for its answer")
		}

		// The batch answer for b lands under the key b's runner recorded.
		if err := store.Save(t.Context(), "b", first.inputs["b"].Hash, b); err != nil {
			t.Fatalf("Save: %v", err)
		}
		second := newRecorder()
		res = mustRun(t, run, []dag.Step{
			{Name: "a", Hash: hashOf("a"), Run: second.runner("a", nil, mustNotRun)},
			{Name: "b", Deps: []string{"a"}, Hash: hashOf("b"), Run: second.runner("b", nil, mustNotRun)},
			{Name: "c", Deps: []string{"b"}, Hash: hashOf("c"), Run: second.runner("c", c, nil)},
		}, Config{Resumer: store})
		wantResults := map[string]dag.StepResult{
			"a": {Status: dag.StatusSucceeded, Output: a, Resumed: true},
			"b": {Status: dag.StatusSucceeded, Output: b, Resumed: true},
			"c": {Status: dag.StatusSucceeded, Output: c},
		}
		if diff := cmp.Diff(wantResults, res.Steps); diff != "" {
			t.Errorf("results after second run (-want, +got):\n%s", diff)
		}
		if diff := cmp.Diff(map[string][]byte{"b": b}, second.inputs["c"].Reads); diff != "" {
			t.Errorf("c reads (-want, +got):\n%s", diff)
		}
	})

	t.Run("the runner sees the hash with or without a Resumer", func(t *testing.T) {
		for _, cfg := range []Config{{}, {Resumer: dagtest.NewMem()}} {
			rec := newRecorder()
			mustRun(t, run, []dag.Step{
				{Name: "hashed", Hash: func(context.Context, dag.Input) (string, error) { return "h", nil }, Run: rec.runner("hashed", nil, nil)},
				{Name: "unhashed", Run: rec.runner("unhashed", nil, nil)},
			}, cfg)
			got := map[string]string{}
			for name, in := range rec.inputs {
				got[name] = in.Hash
			}
			if diff := cmp.Diff(map[string]string{"hashed": "h", "unhashed": ""}, got); diff != "" {
				t.Errorf("Input.Hash with Resumer %v (-want, +got):\n%s", cfg.Resumer != nil, diff)
			}
		}
	})

	t.Run("failures are not saved", func(t *testing.T) {
		store := dagtest.NewMem()
		rec := newRecorder()
		mustRun(t, run, []dag.Step{
			{Name: "bad", Hash: hashOf("bad"), Run: rec.runner("bad", nil, errors.New("boom"))},
		}, Config{Resumer: store})
		if _, found, _ := store.Lookup(t.Context(), "bad", rec.inputs["bad"].Hash); found {
			t.Error("failed step was saved")
		}
	})

	t.Run("hash and store errors fail the step", func(t *testing.T) {
		boom := errors.New(rand.Text())
		tests := []struct {
			name      string
			hash      func(context.Context, dag.Input) (string, error)
			resumer   dag.Resumer
			wantCalls int
		}{{
			name:      "hash",
			hash:      func(context.Context, dag.Input) (string, error) { return "", boom },
			resumer:   dagtest.NewMem(),
			wantCalls: 0,
		}, {
			name:      "lookup",
			hash:      hashOf("a"),
			resumer:   failingResumer{lookupErr: boom},
			wantCalls: 0,
		}, {
			name:      "save",
			hash:      hashOf("a"),
			resumer:   failingResumer{saveErr: boom},
			wantCalls: 1,
		}}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				rec := newRecorder()
				res := mustRun(t, run, []dag.Step{
					{Name: "a", Hash: tc.hash, Run: rec.runner("a", randomBytes(), nil)},
					{Name: "b", Deps: []string{"a"}, Run: rec.runner("b", nil, nil)},
				}, Config{Resumer: tc.resumer})
				want := map[string]dag.Status{"a": dag.StatusFailed, "b": dag.StatusSkipped}
				if diff := cmp.Diff(want, statuses(res)); diff != "" {
					t.Errorf("statuses (-want, +got):\n%s", diff)
				}
				if err := res.Steps["a"].Err; !errors.Is(err, boom) {
					t.Errorf("a.Err: got = %v, want it to wrap %v", err, boom)
				}
				if got := rec.called("a"); got != tc.wantCalls {
					t.Errorf("a runner calls: got = %d, want = %d", got, tc.wantCalls)
				}
			})
		}
	})
}

func testPanic(t *testing.T, run RunFunc) {
	tests := []struct {
		name      string
		run       func(value string) dag.StepRunner
		hash      func(value string) func(context.Context, dag.Input) (string, error)
		resumer   func(value string) panickingResumer
		wantCalls int
		// wantErr is the text a's Err must carry; empty means the panic value.
		wantErr string
	}{{
		name: "runner",
		run: func(value string) dag.StepRunner {
			return func(context.Context, dag.Input) ([]byte, error) { panic(value) }
		},
	}, {
		// t.FailNow in a runner does this.
		name: "goexit",
		run: func(string) dag.StepRunner {
			return func(context.Context, dag.Input) ([]byte, error) { runtime.Goexit(); return nil, nil }
		},
		wantErr: "exited its goroutine",
	}, {
		name: "hash",
		hash: func(value string) func(context.Context, dag.Input) (string, error) {
			return func(context.Context, dag.Input) (string, error) { panic(value) }
		},
	}, {
		name:    "lookup",
		resumer: func(value string) panickingResumer { return panickingResumer{step: "a", lookup: true, value: value} },
	}, {
		name:      "save",
		resumer:   func(value string) panickingResumer { return panickingResumer{step: "a", save: true, value: value} },
		wantCalls: 1,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value := rand.Text()
			rec := newRecorder()
			a := dag.Step{Name: "a", Hash: hashOf("a"), Run: rec.runner("a", randomBytes(), nil)}
			if tc.run != nil {
				a.Run = tc.run(value)
			}
			if tc.hash != nil {
				a.Hash = tc.hash(value)
			}
			store := panickingResumer{}
			if tc.resumer != nil {
				store = tc.resumer(value)
			}
			store.Mem = dagtest.NewMem()
			sibling := randomBytes()
			res := mustRun(t, run, []dag.Step{
				a,
				{Name: "b", Deps: []string{"a"}, Run: rec.runner("b", nil, nil)},
				{Name: "sibling", Hash: hashOf("sibling"), Run: rec.runner("sibling", sibling, nil)},
			}, Config{Resumer: store})

			want := map[string]dag.Status{"a": dag.StatusFailed, "b": dag.StatusSkipped, "sibling": dag.StatusSucceeded}
			if diff := cmp.Diff(want, statuses(res)); diff != "" {
				t.Errorf("statuses (-want, +got):\n%s", diff)
			}
			wantErr := tc.wantErr
			if wantErr == "" {
				wantErr = value
			}
			if err := res.Steps["a"].Err; err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Errorf("a.Err: got = %v, want it to contain %q", err, wantErr)
			}
			if got := rec.called("a"); got != tc.wantCalls {
				t.Errorf("a runner calls: got = %d, want = %d", got, tc.wantCalls)
			}
			if got, found, _ := store.Mem.Lookup(t.Context(), "sibling", rec.inputs["sibling"].Hash); !found || !slices.Equal(got, sibling) {
				t.Errorf("stored sibling output: got = %q (found %v), want = %q", got, found, sibling)
			}
		})
	}
}

func testCancel(t *testing.T, run RunFunc) {
	t.Run("before the run", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		cause := errors.New(rand.Text())
		cancel(cause)
		rec := newRecorder()
		res, err := run(ctx, []dag.Step{
			{Name: "a", Run: rec.runner("a", nil, nil)},
			{Name: "b", Deps: []string{"a"}, Run: rec.runner("b", nil, nil)},
		}, Config{})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		want := map[string]dag.StepResult{
			"a": {Status: dag.StatusSkipped, Err: cause},
			"b": {Status: dag.StatusSkipped, Err: cause},
		}
		if diff := cmp.Diff(want, res.Steps, errorsMatch); diff != "" {
			t.Errorf("results (-want, +got):\n%s", diff)
		}
		if len(rec.calls) != 0 {
			t.Errorf("runner calls: got = %v, want none", rec.calls)
		}
	})

	t.Run("by a step", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		cause := errors.New(rand.Text())
		boom := errors.New(rand.Text())
		rec := newRecorder()
		res, err := run(ctx, []dag.Step{
			{Name: "stopper", Run: func(context.Context, dag.Input) ([]byte, error) {
				cancel(cause)
				return nil, boom
			}},
			{Name: "after", Deps: []string{"stopper"}, Run: rec.runner("after", nil, nil)},
			{Name: "tolerant", OptionalDeps: []string{"stopper"}, Run: rec.runner("tolerant", nil, nil)},
		}, Config{})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		want := map[string]dag.StepResult{
			"stopper":  {Status: dag.StatusFailed, Err: boom},
			"after":    {Status: dag.StatusSkipped, Err: cause},
			"tolerant": {Status: dag.StatusSkipped, Err: cause},
		}
		if diff := cmp.Diff(want, res.Steps, errorsMatch); diff != "" {
			t.Errorf("results (-want, +got):\n%s", diff)
		}
		if len(rec.calls) != 0 {
			t.Errorf("runner calls: got = %v, want none", rec.calls)
		}
	})
	t.Run("a step that finishes as the run is cancelled is still saved", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		cause := errors.New(rand.Text())
		paid := randomBytes()
		store := CtxMem{Mem: dagtest.NewMem()}
		rec := newRecorder()
		var hash string
		res, err := run(ctx, []dag.Step{
			{Name: "paid", Hash: hashOf("paid"), Run: func(_ context.Context, in dag.Input) ([]byte, error) {
				hash = in.Hash
				cancel(cause)
				return paid, nil
			}},
			{Name: "after", Deps: []string{"paid"}, Run: rec.runner("after", nil, nil)},
		}, Config{Resumer: store})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		want := map[string]dag.StepResult{
			"paid":  {Status: dag.StatusSucceeded, Output: paid},
			"after": {Status: dag.StatusSkipped, Err: cause},
		}
		if diff := cmp.Diff(want, res.Steps, errorsMatch); diff != "" {
			t.Errorf("results (-want, +got):\n%s", diff)
		}
		if got, found, _ := store.Lookup(t.Context(), "paid", hash); !found || !slices.Equal(got, paid) {
			t.Errorf("stored paid output: got = %q (found %v), want = %q", got, found, paid)
		}
	})
}

func testConcurrency(t *testing.T, run RunFunc) {
	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "explicit cap", limit: 2, want: 2},
		{name: "default cap of four", limit: 0, want: 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu             sync.Mutex
				inflight, peak int
			)
			steps := make([]dag.Step, 0, 8)
			for i := range 8 {
				steps = append(steps, dag.Step{Name: string(rune('a' + i)), Run: func(context.Context, dag.Input) ([]byte, error) {
					mu.Lock()
					inflight++
					peak = max(peak, inflight)
					mu.Unlock()
					// Yield so a scheduler that ignores the cap has a chance to start more.
					runtime.Gosched()
					mu.Lock()
					inflight--
					mu.Unlock()
					return nil, nil
				}})
			}
			res := mustRun(t, run, steps, Config{MaxConcurrency: tc.limit})
			for name, r := range res.Steps {
				if r.Status != dag.StatusSucceeded {
					t.Errorf("%s: got = %v, want = %v", name, r.Status, dag.StatusSucceeded)
				}
			}
			if peak > tc.want {
				t.Errorf("peak concurrency: got = %d, want <= %d", peak, tc.want)
			}
		})
	}
}
