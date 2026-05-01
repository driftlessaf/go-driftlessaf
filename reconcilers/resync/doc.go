/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package resync shards a time-bucketed resync across many cron firings,
// enqueuing only the slice of keys that belong in the current shard.
//
// The model is:
//
//   - A reconciler chooses a Tick — the cron period at which the resync job
//     fires. Tick is also the shard size and the floor for per-key Periods.
//   - Each source (e.g. a repo) has its own Period: how often its keys must
//     be reconciled. Period must be a positive multiple of Tick.
//   - On every tick, each key is assigned a deterministic minute-of-period
//     in [0, Period/1min). A key is enqueued on the tick whose minute range
//     covers its assignment, with a delay equal to its offset within the
//     tick.
//
// Across one Period, every key is enqueued exactly once. Across periods, the
// minute-of-period assignment reshuffles, so the order in which keys are
// touched changes from cycle to cycle.
//
// A caller constructs one Sharder per cron firing with the job-level
// configuration (now, tick, workqueue client), then calls Process per source
// with that source's Period and candidate keyset:
//
//	sh, err := resync.New(time.Now(), time.Hour, wq)
//	if err != nil { return err }
//	for _, src := range sources {
//	    if err := sh.Process(ctx, src.Period, src.Keys); err != nil {
//	        return err
//	    }
//	}
package resync
