//go:build withauth

/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"context"

	"chainguard.dev/driftlessaf/agents/judge"
)

// judgeConcurrency bounds the number of in-flight primary judge calls across the
// live golden/standalone/benchmark suites. Each suite fans out models x ~22 test
// cases under t.Parallel(), and every case also fans out to the meta-judge during
// eval recording. Left unbounded, the concurrent Vertex streams burst the
// per-model quota buckets (5 req/min default), and calls come back as transient
// errors or degenerate judgements that red the leg with load noise rather than a
// model-quality signal.
//
// Bounding only the primary call is deliberate. The meta-judge calls fire nested
// inside a primary call: RecordTrace runs the eval callbacks before Execute
// returns, and it runs them in parallel, so each primary call spawns up to two
// concurrent meta-judge calls (judge-reasoning + judge-suggestions). Bounding the
// meta-judge here would deadlock: a primary gemini call holding a slot would spawn
// nested meta gemini calls waiting on the same bucket. So the meta-judge stays
// unbounded and leans on its own retry/backoff (judge.Retry in NewGoldenEval).
//
// The consequence: the gemini meta-judge bucket can see up to judgeConcurrency*2
// concurrent calls. That is looser than the primary bound but still strictly
// tighter than #44231, which left the meta-judge fully unbounded and greened its
// leg. If a local run shows the meta bucket still leaking load noise, the remedies
// in order are: lower judgeConcurrency (shrinks the 2x burst too), or add a
// separate meta-judge semaphore on its own bucket (deadlock-free precisely because
// it is a different channel from the primary's).
//
// This is a burst cap, not a rate limit; the executor's retry/backoff paces the
// per-minute rate. The value mirrors the proven fix in #44231 (enricher golden
// tests); tune it from a local run if the leg still shows load noise.
const judgeConcurrency = 4

// judgeSem is a counting semaphore shared across the three suites. The top-level
// Test functions run sequentially (only their subtests call t.Parallel()), so one
// suite holds the semaphore at a time; within a suite it bounds the parallel
// model x case fan-out.
var judgeSem = make(chan struct{}, judgeConcurrency)

// judgeWithLimit runs j.Judge under the shared concurrency bound. It blocks until
// a slot is free (or ctx is done) and releases the slot when Judge returns.
func judgeWithLimit(ctx context.Context, j judge.Interface, req *judge.Request) (*judge.Judgement, error) {
	select {
	case judgeSem <- struct{}{}:
		defer func() { <-judgeSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return j.Judge(ctx, req)
}
