/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/chainguard-dev/clog"
	"golang.org/x/sync/errgroup"

	"chainguard.dev/driftlessaf/workqueue"
)

// Callback is the function that Handle calls to process a particular key.
type Callback func(ctx context.Context, key string, opts workqueue.Options) error

// ServiceCallback returns a Callback that invokes the given service.
func ServiceCallback(client workqueue.WorkqueueServiceClient) Callback {
	return func(ctx context.Context, key string, opts workqueue.Options) error {
		resp, err := client.Process(ctx, &workqueue.ProcessRequest{
			Key:      key,
			Priority: opts.Priority,
		})
		if err != nil {
			return err
		}

		// Handle requeue_after_seconds (backward compatibility).
		// This should NOT be combined with queue_keys.
		if resp.GetRequeueAfterSeconds() > 0 {
			delay := time.Duration(resp.GetRequeueAfterSeconds()) * time.Second
			if resp.GetRequeueFloor() {
				return workqueue.RequeueNotBefore(delay)
			}
			return workqueue.RequeueAfter(delay)
		}

		// Handle queue_keys from response.
		if len(resp.GetQueueKeys()) > 0 {
			keys := make([]workqueue.QueueKey, 0, len(resp.GetQueueKeys()))
			for _, qk := range resp.GetQueueKeys() {
				keys = append(keys, workqueue.QueueKey{
					Key:          qk.GetKey(),
					Priority:     qk.GetPriority(),
					DelaySeconds: qk.GetDelaySeconds(),
				})
			}
			return workqueue.QueueKeys(keys...)
		}

		return nil
	}
}

// Future is a function that can be used to block on the result of a round of
// dispatching work.
type Future func() error

// ErrOrphanRetryBudgetExhausted is the Err an ErrorContext carries when the
// orphan sweep dead-letters a key instead of returning it to the queue: the
// key's lease lapsed while its attempt count already met the dispatcher's
// maxRetry, so another attempt would only repeat the failure that killed the
// last owner. It is reported as an infrastructure failure: the callback never
// answered, so no application error exists to classify.
var ErrOrphanRetryBudgetExhausted = errors.New("orphaned key exhausted its retry budget")

// budgetedOrphan is the surface the orphan sweep needs to dead-letter an
// observed key over budget: its attempt count and the dead-letter move. Every
// in-tree queue's in-progress key carries both; a queue whose observed keys
// do not is swept as before, by requeueing.
type budgetedOrphan interface {
	workqueue.ObservedInProgressKey
	GetAttempts() int
	Deadletter(context.Context) error
}

// Handle is a synchronous form of HandleAsync.
func Handle(ctx context.Context, wq workqueue.Interface, concurrency, batchSize int, f Callback, opts ...Option) error {
	return HandleAsync(ctx, wq, concurrency, batchSize, f, 0, opts...)()
}

// HandleAsync initiates a single iteration of the dispatcher, possibly invoking
// the callback for several different keys.  It returns a future that can be
// used to block on the result.
func HandleAsync(ctx context.Context, wq workqueue.Interface, concurrency, batchSize int, f Callback, maxRetry int, opts ...Option) Future {
	cfg := applyOptions(opts)
	identity := wq.Identity()
	if cfg.ownerConcurrency > 0 && identity == "" {
		return func() error { return fmt.Errorf("owner concurrency requires a non-empty queue identity") }
	}
	// Enumerate the state of the queue. Capacity-aware implementations can
	// avoid a full listing of the unbounded queued set when all worker slots are
	// already occupied, while still reading every in-progress key for orphan
	// recovery and any bounded telemetry reads they need.
	// Capacity-aware queues need the dispatcher's total capacity to decide
	// whether queued work can affect this pass. Owner-specific limits are
	// applied after enumeration and must not suppress a globally available slot.
	totalCapacity := concurrency
	var wip []workqueue.ObservedInProgressKey
	var next []workqueue.QueuedKey
	var err error
	if bounded, ok := wq.(workqueue.CapacityAware); ok {
		wip, next, _, err = bounded.EnumerateWithCapacity(ctx, totalCapacity)
	} else {
		wip, next, _, err = wq.Enumerate(ctx)
	}
	if err != nil {
		return func() error { return fmt.Errorf("enumerate() = %w", err) }
	}

	if batchSize <= 0 || batchSize > concurrency {
		batchSize = concurrency
	}

	eg := errgroup.Group{}

	// Remove any orphaned work by returning it to the queue, or by
	// dead-lettering it when its attempt count already meets maxRetry: the
	// owner that claimed the last attempt died without reporting (a callback
	// crash takes the dispatcher down with it before Deadletter can run), so
	// the lease lapsed with the budget spent and a requeue would only claim
	// the same key again, attempts+1, and die the same way. Without this the
	// budget is unreachable for exactly the failures that kill the process.
	// Use context.WithoutCancel to ensure the move completes even if the
	// parent context is canceled.
	activeKeys := make(map[string]struct{}, len(wip))
	ownerWIP := 0
	for _, x := range wip {
		if !x.IsOrphaned() {
			activeKeys[x.Name()] = struct{}{}
			if cfg.ownerConcurrency > 0 && x.Owner() == identity {
				ownerWIP++
			}
			continue
		}
		eg.Go(func() error {
			sweepCtx := context.WithoutCancel(ctx)
			if b, ok := x.(budgetedOrphan); ok && maxRetry > 0 {
				if attempts := b.GetAttempts(); attempts >= maxRetry {
					clog.InfoContextf(ctx, "Orphaned key %q has exhausted its retry budget (%d/%d), dead-lettering instead of requeueing", x.Name(), attempts, maxRetry)
					if err := b.Deadletter(sweepCtx); err != nil {
						if errors.Is(err, workqueue.ErrDeadletterSkipped) {
							// The lease changed since the listing or the object is
							// gone: the key is not ours to retire, so nothing to
							// report as dead-lettered.
							clog.InfoContextf(ctx, "Orphaned key %q left alone, its lease changed since observation: %v", x.Name(), err)
							return nil
						}
						return fmt.Errorf("deadletter(orphan over max retries) = %w", err)
					}
					cfg.errors.emit(sweepCtx, ErrorContext{
						Key:            x.Name(),
						Err:            ErrOrphanRetryBudgetExhausted,
						Attempts:       attempts,
						Action:         ErrorDeadLettered,
						Infrastructure: true,
					})
					return nil
				}
			}
			return x.Requeue(sweepCtx)
		})
	}

	// If our open slots are filled, then we can't launch any new work!
	// We explicitly check this here because if nWIP grows larger than
	// concurrency then the subtraction below will underflow and we'll
	// start to queue work without bounds.
	nWIP := len(activeKeys)
	if nWIP >= concurrency {
		return eg.Wait // Should generally be a no-op.
	}
	if cfg.ownerConcurrency > 0 && ownerWIP >= cfg.ownerConcurrency {
		return eg.Wait
	}

	// Attempt to launch a new piece of work for each open slot we have available
	// which is: N - active.
	openSlots := concurrency - nWIP
	if cfg.ownerConcurrency > 0 {
		openSlots = min(openSlots, cfg.ownerConcurrency-ownerWIP)
	}
	launchLimit := min(batchSize, openSlots)

	// An identity means sibling dispatchers may be reading this same ordering.
	if identity != "" {
		next = orderCandidates(next, activeKeys, cfg.windowFactor*launchLimit, cfg.shuffle)
	}

	idx, launched := 0, 0
	for ; idx < len(next) && launched < launchLimit; idx++ {
		nextKey := next[idx]

		// If the next key is already in progress, then move to the next candidate.
		if _, ok := activeKeys[nextKey.Name()]; ok {
			continue
		}

		// At this point, we know that nextKey gets launched.  There are two paths below:
		// 1. One is where we lose the race and someone else launches it, and
		// 2. The other is where we launch it.
		// By incrementing the counter here, we ensure we don't overlaunch keys due to a race.
		launched++

		// This is done in a Go routine so that we can process keys concurrently.
		eg.Go(func() error {
			// Start the work, moving it to be in-progress. If we are unsuccessful starting
			// the work, then someone beat us to it, so move on to the next key.
			oip, err := nextKey.Start(ctx)
			if err != nil {
				clog.DebugContextf(ctx, "Failed to start key %q: %v", nextKey.Name(), err)
				return nil
			}

			// Attempt to perform the actual reconciler invocation.
			err = f(oip.Context(), oip.Name(), workqueue.Options{
				Priority: oip.Priority(),
			})

			// Use context.WithoutCancel for all cleanup operations to ensure they
			// complete even if the parent context is canceled (e.g., SIGTERM).
			// This prevents work items from getting stuck in "in-progress" state
			// when the worker is terminated.
			cleanupCtx := context.WithoutCancel(ctx)

			// Check if this is a requeue error with custom delay
			if delay, floor, ok := workqueue.GetRequeueOptions(err); ok {
				// A floor can't be undercut by events or resync, so clamp it to
				// MaximumRequeueFloor to keep an over-large floor from starving the
				// key. RequeueAfter (non-floor) is left alone: it stays undercuttable.
				if floor && delay > workqueue.MaximumRequeueFloor {
					clog.InfoContextf(ctx, "Key %q floored requeue delay %v exceeds MaximumRequeueFloor %v, clamping", oip.Name(), delay, workqueue.MaximumRequeueFloor)
					delay = workqueue.MaximumRequeueFloor
				}
				clog.InfoContextf(ctx, "Key %q requested requeue after %v (floor=%t)", oip.Name(), delay, floor)
				if err := oip.RequeueWithOptions(cleanupCtx, workqueue.Options{
					Priority:       oip.Priority(),
					Delay:          delay,
					NotBeforeFloor: floor,
				}); err != nil {
					return fmt.Errorf("requeue(after delay request) = %w", err)
				}
				return nil
			}

			// Extract queue keys from sentinel error (if any).
			// If present, this is a success case - queue keys BEFORE completing current key.
			if queueKeys := workqueue.GetQueueKeys(err); len(queueKeys) > 0 {
				// Queue all keys BEFORE completing current key.
				// If any queue operation fails, fail the entire operation.
				// Note: If current key is in queueKeys, it enters "dual state" (queued + in-progress)
				// and will be processed again after we complete the current in-progress work.
				for _, qk := range queueKeys {
					opts := workqueue.Options{
						Priority: qk.Priority,
					}
					if qk.DelaySeconds > 0 {
						opts.NotBefore = time.Now().Add(time.Duration(qk.DelaySeconds) * time.Second)
					}

					if err := wq.Queue(cleanupCtx, qk.Key, opts); err != nil {
						// Fail the operation - current key will be retried
						clog.WarnContextf(ctx, "Failed to queue key %q: %v", qk.Key, err)
						if requeueErr := oip.Requeue(cleanupCtx); requeueErr != nil {
							return fmt.Errorf("requeue(after queue failure) = %w", requeueErr)
						}
						return nil // Don't bubble up as dispatcher error
					}
					clog.InfoContextf(ctx, "Queued key %q (priority=%d, delay=%ds)", qk.Key, qk.Priority, qk.DelaySeconds)
				}
			} else if err != nil {
				// Real error (not a sentinel) - handle failure.
				clog.WarnContextf(ctx, "Failed callback for key %q: %v", oip.Name(), err)
				attempts := oip.GetAttempts()

				if ctx.Err() != nil {
					// The dispatch context ended while the callback ran: the
					// dispatcher is shutting down or its dispatch request was cut,
					// so the callback was interrupted rather than failed. That is
					// infrastructure's doing and must not consume the key's
					// dead-letter budget. Requeue with Delay semantics, which reset
					// the attempt count, on the drain schedule so the key returns
					// shortly on a live dispatcher.
					delay := interruptionDelay()
					clog.InfoContextf(ctx, "Key %q interrupted by dispatcher shutdown (attempt %d), requeueing in %v without consuming an attempt", oip.Name(), attempts, delay)
					if err := oip.RequeueWithOptions(cleanupCtx, workqueue.Options{Priority: oip.Priority(), Delay: delay}); err != nil {
						return fmt.Errorf("requeue(after interrupted callback) = %w", err)
					}
					cfg.errors.emit(cleanupCtx, ErrorContext{
						Key:            oip.Name(),
						Err:            err,
						Attempts:       attempts,
						Action:         ErrorRequeued,
						Infrastructure: true,
					})
					return nil
				}

				// If maxRetry is configured and we've reached or exceeded it, use Deadletter() instead of Requeue()
				if maxRetry > 0 && attempts >= maxRetry {
					clog.InfoContextf(ctx, "Key %q has reached max retry limit (%d/%d), failing permanently",
						oip.Name(), attempts, maxRetry)

					if err := oip.Deadletter(cleanupCtx); err != nil {
						if errors.Is(err, workqueue.ErrDeadletterSkipped) {
							// Another attempt already moved the key; there is no
							// dead-letter of ours to report.
							clog.InfoContextf(ctx, "Key %q left alone, another attempt moved it: %v", oip.Name(), err)
							return nil
						}
						return fmt.Errorf("fail(after reaching max retries) = %w", err)
					}
					cfg.errors.emit(cleanupCtx, ErrorContext{
						Key:            oip.Name(),
						Err:            err,
						Attempts:       attempts,
						Action:         ErrorDeadLettered,
						Infrastructure: workqueue.IsInfrastructureError(err),
					})
				} else if d := workqueue.GetDeadLetterDetails(err); d != nil {
					// Checked BEFORE the plain non-retriable branch: a
					// DeadLetterError carries NoRetryDetails too (so an older
					// dispatcher degrades to the drop, never a retry loop), and
					// matching that first here would silently complete a key the
					// callback asked to surface durably.
					clog.InfoContextf(ctx, "Key %q is marked for immediate dead-letter - reason: %s, err: %v", oip.Name(), d.GetMessage(), err)
					if err := oip.Deadletter(cleanupCtx); err != nil {
						if errors.Is(err, workqueue.ErrDeadletterSkipped) {
							clog.InfoContextf(ctx, "Key %q left alone, another attempt moved it: %v", oip.Name(), err)
							return nil
						}
						return fmt.Errorf("deadletter(after dead-letter error) = %w", err)
					}
					cfg.errors.emit(cleanupCtx, ErrorContext{
						Key:                oip.Name(),
						Err:                err,
						Attempts:           attempts,
						Action:             ErrorDeadLettered,
						NonRetriableReason: d.GetMessage(),
						Infrastructure:     workqueue.IsInfrastructureError(err),
					})
				} else if d := workqueue.GetNonRetriableDetails(err); d != nil {
					clog.InfoContextf(ctx, "Key %q is marked as non-retriable - reason: %s, err: %v", oip.Name(), d.GetMessage(), err)
					// If the error is marked as non-retriable, we should not requeue it.
					if err := oip.Complete(cleanupCtx); err != nil {
						return fmt.Errorf("complete(after non-retriable error) = %w", err)
					}
					cfg.errors.emit(cleanupCtx, ErrorContext{
						Key:                oip.Name(),
						Err:                err,
						Attempts:           attempts,
						Action:             ErrorDropped,
						NonRetriableReason: d.GetMessage(),
					})
				} else {
					// Every retriable failure requeues on a jittered doubling
					// curve: a fast first step keeps races and transient blips
					// cheap, and the widening keeps persistent failures
					// (infrastructure storms, deterministic errors awaiting a
					// fix) from burning the dead-letter budget in minutes.
					// Whether the failure was infrastructure no longer changes
					// scheduling — recovery horizons for both classes are
					// hours-scale — it is recorded for observability only.
					// BackoffDelay preserves the attempt count, so the
					// dead-letter cutoff above stays reachable. A WithBackoff
					// hook replaces the default curve when it returns a
					// positive duration.
					var delay time.Duration
					if cfg.backoff != nil {
						delay = cfg.backoff(attempts)
					}
					if delay <= 0 {
						delay = retryBackoff(attempts)
						if jitter := delay / 2; jitter > 0 {
							delay += rand.N(jitter) //nolint:gosec // G404: jitter, not security-sensitive
						}
					}
					infra := workqueue.IsInfrastructureError(err)
					clog.InfoContextf(ctx, "Key %q failed (attempt %d, infrastructure=%t), requeueing with %v backoff", oip.Name(), attempts, infra, delay)
					if err := oip.RequeueWithOptions(cleanupCtx, workqueue.Options{BackoffDelay: delay}); err != nil {
						return fmt.Errorf("requeue(after failed callback) = %w", err)
					}
					cfg.errors.emit(cleanupCtx, ErrorContext{
						Key:            oip.Name(),
						Err:            err,
						Attempts:       attempts,
						Action:         ErrorRequeued,
						Infrastructure: infra,
					})
				}
				return nil // This isn't an error in the dispatcher itself.
			}

			// Delete the in-progress key (stops heartbeat).
			// Use context.WithoutCancel to ensure completion even if context is canceled.
			if err := oip.Complete(cleanupCtx); err != nil {
				return fmt.Errorf("complete() = %w", err)
			}
			return nil
		})
	}
	clog.InfoContextf(ctx, "Launched %d new keys (wip: %d, batch: %d)", launched, nWIP, batchSize)

	// Return the future to wait on outstanding work, then drain any
	// in-flight error events so they are not lost on shutdown.
	return func() error {
		err := eg.Wait()
		cfg.errors.drain()
		return err
	}
}

// DefaultCandidateWindowFactor scales the shuffled candidate window with the
// number of keys a pass can launch: W = DefaultCandidateWindowFactor * L.
//
// The factor trades collisions against ordering. Raising it lowers the chance
// two dispatchers reach for the same key, and raises how long a key can sit in
// the window, because each key is picked with probability L/W per pass and so
// waits the factor in passes when it is the only dispatcher. Measured for
// three dispatchers, as lost share of claim attempts against that wait:
//
//	factor   aggregate   slowest   wait (1 dispatcher)
//	    16       6.1%     11.2%             16 passes
//	    24       4.0%      8.0%             24 passes
//	    32       3.2%      6.5%             32 passes
//	    48       2.0%      4.2%             47 passes
//	    64       1.7%      3.4%             60 passes
//
// 48 is the smallest that holds the slowest dispatcher under 5%, which is the
// bar this was built to, and it sits at the knee: 32 to 48 buys 2.3 points for
// 14 passes, 48 to 64 buys 0.8 points for another 14. Beyond about 96 the
// curve flattens because the window reaches the enumeration limit.
//
// The loss figures assume every dispatcher has the same L and a window shorter
// than the enumerated list. Where per-owner limits make L differ, or L grows
// until the window covers the whole list, loss rises toward
// 1-(1-L/limit)^(k-1). Every regime still beats taking the head, where all but
// one claim per contested key is lost.
const DefaultCandidateWindowFactor = 48

// orderCandidates returns the launchable keys from next, with the head of each
// priority run shuffled so that dispatchers sharing a queue tend to pick
// different keys. Without it every dispatcher reads the same ordering and
// races for the same head, and all but one lose the slot for the pass.
//
// Keys in active are dropped rather than left in place, so they cannot spend
// window positions that no dispatcher can claim. A key appears both queued and
// in progress when the winner of an earlier claim failed to delete the queued
// object. With no shuffle source or no window the input is returned as it
// came, active keys included, and the caller's own skip handles them.
//
// Relative order across priorities is preserved, so a lower-priority key never
// precedes a higher-priority one. Within one priority only the first window
// keys are permuted, which bounds how far a key can be passed over in a single
// pass.
func orderCandidates(next []workqueue.QueuedKey, active map[string]struct{}, window int, shuffle func(n int, swap func(i, j int))) []workqueue.QueuedKey {
	if shuffle == nil || window <= 0 {
		return next
	}

	eligible := make([]workqueue.QueuedKey, 0, len(next))
	for _, key := range next {
		if _, ok := active[key.Name()]; ok {
			continue
		}
		eligible = append(eligible, key)
	}

	for start := 0; start < len(eligible); {
		end := start + 1
		for end < len(eligible) && eligible[end].Priority() == eligible[start].Priority() {
			end++
		}
		if n := min(window, end-start); n > 1 {
			run := eligible[start : start+n]
			shuffle(n, func(i, j int) { run[i], run[j] = run[j], run[i] })
		}
		start = end
	}
	return eligible
}

// retryBackoff returns the base backoff for a failed dispatch on the given
// attempt: workqueue.BackoffPeriod doubling per recorded attempt, capped at
// workqueue.MaximumBackoffPeriod. Attempt counts below one (possible when a
// key's attempt metadata is malformed) yield the base period.
func retryBackoff(attempts int) time.Duration {
	delay := workqueue.BackoffPeriod
	for range attempts - 1 {
		if delay >= workqueue.MaximumBackoffPeriod {
			break
		}
		delay *= 2
	}
	return min(delay, workqueue.MaximumBackoffPeriod)
}

// interruptionDelay is the requeue delay for a callback the dispatcher's own
// shutdown interrupted: the drain schedule, jittered so a retiring dispatcher's
// in-flight keys do not all return at once.
func interruptionDelay() time.Duration {
	delay := workqueue.DrainRequeueDelay
	if workqueue.DrainRequeueJitter > 0 {
		delay += rand.N(workqueue.DrainRequeueJitter) //nolint:gosec // G404: jitter, not security-sensitive
	}
	return delay
}
