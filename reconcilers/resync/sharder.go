/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package resync

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	"golang.org/x/sync/errgroup"
)

const defaultConcurrency = 32

// Option configures a Sharder.
type Option func(*options)

type options struct {
	now         func() time.Time
	concurrency int
}

// WithConcurrency bounds the number of in-flight workqueue enqueues per
// Process call. n must be positive. Default is 32.
func WithConcurrency(n int) Option {
	return func(o *options) { o.concurrency = n }
}

// WithNow overrides the wall-clock function used by the Sharder. It is
// called once at construction to anchor the shard window (the "start"
// instant), and again at each enqueue so the produced DelaySeconds
// compensates for time spent enumerating keys and contacting the workqueue.
// Production code can rely on the default of time.Now; tests pass a
// controllable clock. The function must be safe for concurrent use.
func WithNow(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// Sharder enqueues keys onto a workqueue at their resync-shard offset.
//
// A Sharder is constructed once per cron firing with the job-level tick and
// a workqueue client. Each Process call supplies a per-keyset resync period
// and the keys to consider for that period.
type Sharder struct {
	now         func() time.Time
	start       time.Time
	tick        time.Duration
	wq          workqueue.Client
	concurrency int
}

// New configures a Sharder.
//
//   - tick is the cron period (= shard size = per-key Period floor). It must
//     be positive and a whole number of minutes.
//   - wq is the workqueue client that Process will enqueue onto.
//
// The wall-clock used to anchor the shard window and to compensate enqueue
// delays defaults to time.Now and can be overridden via [WithNow].
func New(tick time.Duration, wq workqueue.Client, opts ...Option) (*Sharder, error) {
	if tick <= 0 {
		return nil, fmt.Errorf("resync: tick must be positive, got %v", tick)
	}
	if tick%time.Minute != 0 {
		return nil, fmt.Errorf("resync: tick must be a whole number of minutes, got %v", tick)
	}
	if wq == nil {
		return nil, errors.New("resync: workqueue client must be non-nil")
	}
	o := options{
		now:         time.Now,
		concurrency: defaultConcurrency,
	}
	for _, opt := range opts {
		opt(&o)
	}
	if o.now == nil {
		return nil, errors.New("resync: WithNow must be non-nil")
	}
	if o.concurrency <= 0 {
		return nil, fmt.Errorf("resync: concurrency must be positive, got %d", o.concurrency)
	}
	return &Sharder{
		now:         o.now,
		start:       o.now(),
		tick:        tick,
		wq:          wq,
		concurrency: o.concurrency,
	}, nil
}

// Process enqueues each key in keys whose minute-of-period (computed under
// period) falls in the current shard. Each in-shard key is assigned a target
// instant inside the shard window; the produced DelaySeconds is
// max(0, target - now()) at the moment of enqueue, so time spent enumerating
// keys and contacting the workqueue subtracts from the delay rather than
// pushing the target forward. period must be a positive multiple of the tick
// supplied to New.
//
// The hash assignment is salted by periodStart (start truncated to period),
// so the per-key minute-of-period is stable within a period and reshuffles
// between periods.
//
// Process fans out workqueue enqueues up to the Sharder's concurrency limit;
// the first error cancels the remaining enqueues and is returned.
func (s *Sharder) Process(ctx context.Context, period time.Duration, keys map[string]struct{}) error {
	if period <= 0 {
		return fmt.Errorf("resync: period must be positive, got %v", period)
	}
	if period%s.tick != 0 {
		return fmt.Errorf("resync: tick (%v) must divide period (%v) evenly", s.tick, period)
	}

	periodStart := s.start.Truncate(period)
	tickStart := s.start.Truncate(s.tick)
	shardStartMin := int64(tickStart.Sub(periodStart) / time.Minute)
	tickMinutes := int64(s.tick / time.Minute)
	periodMinutes := uint64(period / time.Minute)
	periodSalt := uint64(periodStart.Unix()) //nolint:gosec // G115: Unix() is interpreted as bit pattern; sign is irrelevant for hashing.

	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(s.concurrency)
	for key := range keys {
		keyMin := int64(bucket(periodSalt, key, periodMinutes)) //nolint:gosec // G115: bucket result is in [0, periodMinutes), bounded.
		if keyMin < shardStartMin || keyMin >= shardStartMin+tickMinutes {
			continue
		}
		target := s.start.Add(time.Duration(keyMin-shardStartMin) * time.Minute)
		eg.Go(func() error {
			// Compute DelaySeconds against now() at the moment of enqueue,
			// not against start. Tree walks, App enumeration, and workqueue
			// RPCs can add minutes of latency between New and this point;
			// using start.Add(delay) directly would push every key forward
			// by that latency and cause drift across cycles. Anchoring on
			// the absolute target instant (start + intra-shard offset) and
			// subtracting the latest now() lets the latency subtract from
			// the wait instead of accumulating onto it.
			if _, err := s.wq.Process(egCtx, &workqueue.ProcessRequest{
				Key:          key,
				DelaySeconds: max(0, int64(target.Sub(s.now()).Seconds())),
			}); err != nil {
				return fmt.Errorf("enqueue %q: %w", key, err)
			}
			clog.InfoContextf(egCtx, "enqueued %q at %s", key, target.Format(time.RFC3339))
			return nil
		})
	}
	return eg.Wait()
}

// bucket assigns key a deterministic minute-of-period in [0, periodMinutes),
// salted by periodSalt so the assignment reshuffles between periods.
func bucket(periodSalt uint64, key string, periodMinutes uint64) uint64 {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], periodSalt)

	h := sha256.New()
	h.Write(buf[:])
	h.Write([]byte{0x00})
	h.Write([]byte(key))
	sum := h.Sum(nil)

	return binary.BigEndian.Uint64(sum[:8]) % periodMinutes
}
