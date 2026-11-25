/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package dag runs a graph of named steps in dependency order with bounded
// concurrency.
//
// A [Step] names the steps it depends on and supplies a [StepRunner]. [New]
// rejects a graph with an empty or duplicate name, a missing runner, an edge to
// an unknown step, or a cycle over either kind of edge. [Graph.Run] then starts
// each step once all of its dependencies have finished, handing it their
// outputs in [Input.Reads], and reports one [StepResult] per step. At most four
// steps run at once unless [WithMaxConcurrency] says otherwise.
//
// # End states
//
// Once a step's dependencies have finished, its end state is decided in this
// order: the run was cancelled ([StatusSkipped]); a required dependency failed
// or was skipped ([StatusSkipped]); a dependency was deferred or waiting
// ([StatusWaiting]); the [Resumer] has a stored output (resumed); otherwise the
// runner runs.
//
// Failure stays local: a failed step skips only the steps that depend on it
// through [Step.Deps]. An edge in [Step.OptionalDeps] tolerates the failure,
// and the step runs without that input. To stop a whole run, cancel the context
// passed to Run, with [context.WithCancelCause] to give a reason; every step
// that has not started is skipped and reports the cause in its Err.
//
// A step whose runner, Hash or [Resumer] panicked or exited its goroutine (with
// [runtime.Goexit], as t.FailNow does) fails like a returned error; a panic's
// value and stack go in its Err.
//
// # Deferral
//
// A runner returns an error wrapping [ErrDeferred] when its answer will arrive
// later, for example from a batch job. The step ends [StatusDeferred], and its
// dependents, over either kind of edge, end [StatusWaiting] without running.
// Independent steps keep running. A later Run over the same graph picks the
// step up again.
//
// # Resume
//
// A step with a [Step.Hash] computes a hash of everything its output depends
// on, and its runner sees it in [Input.Hash], so a deferred step can record the
// key its answer will be stored under. With [WithResumer], Run asks
// [Resumer.Lookup] for an output stored under the step's name and that hash
// before running the step, and passes each fresh success to [Resumer.Save]. An
// error from Hash, Lookup or Save fails the step. Save's ctx carries the run's
// values but no deadline or cancellation, so a finished step's output is kept
// after the run is cancelled; Save must bound its own wait.
//
// # Retry and iteration
//
// A graph never has a back edge, so a step that must repeat does so inside its
// runner. [Retry] re-runs a runner on transient errors with the backoff of
// package retry. [Iterate] runs a step function until it reports done, up to a
// cap, handing each pass the previous pass's output so a judge gate can revise
// a draft and score it again. Both return a [StepRunner], and they compose:
// Retry around Iterate retries the whole loop, and Iterate can call a Retry
// runner on each pass. Neither repeats a deferred step or starts work once the
// run is cancelled. A capped Iterate fails its step under [Graph.Run]; see
// [Iterate] for keeping its best effort.
package dag
