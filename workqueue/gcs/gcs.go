/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/chainguard-dev/clog"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"chainguard.dev/driftlessaf/workqueue"
)

// ClientInterface is an interface that abstracts the GCS client.
type ClientInterface interface {
	Object(name string) *storage.ObjectHandle
	Objects(ctx context.Context, q *storage.Query) *storage.ObjectIterator
}

// Option configures a GCS-backed workqueue created by NewWorkQueue.
type Option func(*wq)

// WithName sets the queue_name label applied to every Prometheus metric this
// workqueue emits. Use it to disambiguate multiple workqueues running in the
// same Cloud Run service (which share K_SERVICE / K_REVISION). Defaults to "".
func WithName(name string) Option {
	return func(w *wq) { w.name = name }
}

// NewWorkQueue creates a new GCS-backed workqueue.
func NewWorkQueue(client ClientInterface, limit int, opts ...Option) workqueue.Interface {
	w := &wq{
		client: client,
		limit:  limit,
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

type wq struct {
	client ClientInterface
	limit  int
	// name is the queue_name label value applied to all metrics emitted by
	// this queue; "" if WithName was not provided.
	name string
}

// baseLabels returns the {service_name, revision_name, queue_name} labels that
// every workqueue metric carries.
func (w *wq) baseLabels() prometheus.Labels {
	return prometheus.Labels{
		"service_name":  baseServiceName,
		"revision_name": baseRevisionName,
		"queue_name":    w.name,
	}
}

var _ workqueue.Interface = (*wq)(nil)

// RefreshInterval is the period on which we refresh the lease of owned objects
// It is surfaced as a global, so that it can be mutated by tests and exposed as
// a flag by binaries wrapping this library.  However, binary authors should use
// caution to pass consistent values to the key ingress and dispatchers, or they
// may see unexpected behavior.
// TODO(mattmoor): What's the right balance here?
var RefreshInterval = 5 * time.Minute

// heartbeatRetryInterval is the period on which the lease heartbeat retries
// after a transient storage error, until the lease would expire. It is a
// variable so tests can shorten it.
var heartbeatRetryInterval = 30 * time.Second

// The minimum number of attempts before tracking work attempts.
// This is to minimize the cardinality of the metric.
var TrackWorkAttemptMinThreshold = 20

const (
	queuedPrefix              = "queued/"
	inProgressPrefix          = "in-progress/"
	deadLetterPrefix          = "dead-letter/"
	expirationMetadataKey     = "lease-expiration"
	attemptsMetadataKey       = "attempts"
	priorityMetadataKey       = "priority"
	notBeforeMetadataKey      = "not-before"
	notBeforeFloorMetadataKey = "not-before-floor"
	notBeforeFloorValue       = "true"
	failedTimeMetadataKey     = "failed-time"
	lastAttemptedKey          = "last-attempted"
)

var noPriority = fmt.Sprintf("%08d", 0)
var noNotBefore = time.Time{}.UTC().Format(time.RFC3339)

// Queue implements workqueue.Interface.
func (w *wq) Queue(ctx context.Context, key string, opts workqueue.Options) error {
	writer := w.client.Object(fmt.Sprintf("%s%s", queuedPrefix, key)).If(storage.Conditions{
		DoesNotExist: true,
	}).NewWriter(ctx)

	writer.Metadata = map[string]string{
		// Zero-pad the priority to 8 digits to ensure lexicographic ordering,
		// so that we don't have to parse it to order things.
		priorityMetadataKey: fmt.Sprintf("%08d", opts.Priority),
	}
	writer.Metadata[notBeforeMetadataKey] = opts.NotBefore.UTC().Format(time.RFC3339)
	if opts.NotBeforeFloor {
		writer.Metadata[notBeforeFloorMetadataKey] = notBeforeFloorValue
	}

	mAddedKeys.With(w.baseLabels()).Add(1)

	if _, err := writer.Write([]byte("")); err != nil {
		clog.WarnContextf(ctx, "Queue: Write failed for key %q: %v", key, err)
		return fmt.Errorf("Write() = %w", err)
	}
	if exists, err := checkPreconditionFailedOk(writer.Close()); err != nil {
		clog.WarnContextf(ctx, "Queue: Close failed for key %q: %v", key, err)
		return fmt.Errorf("Close() = %w", err)
	} else if exists {
		clog.DebugContextf(ctx, "Key %q already exists", key)
		mDedupedKeys.With(w.baseLabels()).Add(1)

		if err := updateMetadata(ctx, w.client, key, writer.Metadata); err != nil {
			if errors.Is(err, storage.ErrObjectNotExist) {
				clog.InfoContextf(ctx, "Key %q was deleted before we could fetch the duplicate, recursing.", key)
				return w.Queue(ctx, key, opts)
			}
			return fmt.Errorf("updateMetadata() = %w", err)
		}
	}
	return nil
}

func updateMetadata(ctx context.Context, client ClientInterface, key string, metadata map[string]string) error {
	attrs, err := client.Object(fmt.Sprintf("%s%s", queuedPrefix, key)).Attrs(ctx)
	if err != nil {
		clog.WarnContextf(ctx, "updateMetadata: Attrs failed for key %q: %v", key, err)
		return fmt.Errorf("Attrs() = %w", err)
	}
	// Inialialize the metadata map if it's nil.
	if attrs.Metadata == nil {
		attrs.Metadata = make(map[string]string, 2)
	}
	update := false
	// Always choose the highest priority.
	if p, ok := attrs.Metadata[priorityMetadataKey]; !ok || p < metadata[priorityMetadataKey] {
		clog.InfoContextf(ctx, "Updating %s priority from %q to %q", key, p, metadata[priorityMetadataKey])
		attrs.Metadata[priorityMetadataKey] = metadata[priorityMetadataKey]
		update = true
	}
	// not-before merge, following the floor semantics on Options.NotBeforeFloor.
	// RFC3339 UTC timestamps sort lexicographically by time, so string compares
	// stand in for time compares; an absent existing value ("") sorts earliest,
	// which is correct (there is no floor to honor yet).
	existingNB, hasNB := attrs.Metadata[notBeforeMetadataKey]
	incomingNB := metadata[notBeforeMetadataKey]
	existingFloor := attrs.Metadata[notBeforeFloorMetadataKey] == notBeforeFloorValue
	incomingFloor := metadata[notBeforeFloorMetadataKey] == notBeforeFloorValue
	switch {
	case incomingFloor && existingFloor:
		// Two floors: take the later (max) not-before.
		if existingNB < incomingNB {
			clog.InfoContextf(ctx, "Raising %s floor not-before from %q to %q", key, existingNB, incomingNB)
			attrs.Metadata[notBeforeMetadataKey] = incomingNB
			update = true
		}
	case incomingFloor:
		// Incoming floor over a non-floor entry: the floor wins outright,
		// replacing the (undercuttable) not-before in either direction.
		clog.InfoContextf(ctx, "Floor %q replaces %s non-floor not-before %q", incomingNB, key, existingNB)
		attrs.Metadata[notBeforeMetadataKey] = incomingNB
		attrs.Metadata[notBeforeFloorMetadataKey] = notBeforeFloorValue
		update = true
	case existingFloor:
		// Existing floor, non-floor incoming: keep it untouched (no undercut, no postpone).
	case hasNB && existingNB > incomingNB:
		// Neither is a floor: the earliest not-before wins.
		clog.InfoContextf(ctx, "Updating %s not-before from %q to %q", key, existingNB, incomingNB)
		attrs.Metadata[notBeforeMetadataKey] = incomingNB
		update = true
	}
	if update {
		if _, err := client.Object(fmt.Sprintf("%s%s", queuedPrefix, key)).Update(ctx, storage.ObjectAttrsToUpdate{
			Metadata: attrs.Metadata,
		}); err != nil {
			clog.WarnContextf(ctx, "updateMetadata: Update failed for key %q: %v", key, err)
			return fmt.Errorf("Update() = %w", err)
		}
	}
	return nil
}

// isNotFound reports whether err indicates the object (or the pinned
// generation of it) does not exist.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return true
	}
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}

// lostOwnership reports whether err indicates that the in-progress object no
// longer matches the generation this process leased: either a precondition
// failed (the object was replaced or modified by another actor) or the object
// is gone entirely. Transient errors (rate limiting, server errors, network
// blips) are NOT ownership loss — the object still carries our lease.
func lostOwnership(err error) bool {
	if isNotFound(err) {
		return true
	}
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusPreconditionFailed
}

func checkPreconditionFailedOk(err error) (bool, error) {
	// No error is OK.
	if err == nil {
		return false, nil
	}
	// If the error is a googleapi.Error, and it's a PreconditionFailed,
	// then it's OK.
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		if gerr.Code == http.StatusPreconditionFailed {
			return true, nil
		}
	}
	return false, err
}

// Enumerate implements workqueue.Interface.
func (w *wq) Enumerate(ctx context.Context) ([]workqueue.ObservedInProgressKey, []workqueue.QueuedKey, []workqueue.DeadLetteredKey, error) {
	labels := w.baseLabels()

	start := time.Now()
	defer func() {
		mEnumerateLatency.With(labels).Observe(time.Since(start).Seconds())
	}()

	iter := w.client.Objects(ctx, nil)

	wip := make([]workqueue.ObservedInProgressKey, 0, w.limit)
	qd := make([]*queuedKey, 0, w.limit+1)
	var dl []workqueue.DeadLetteredKey

	queued, notbefore, deadlettered := 0, 0, 0
	maxAttempts := 0 // Track the maximum number of attempts
	for {
		objAttrs, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		} else if err != nil {
			return nil, nil, nil, fmt.Errorf("Next() = %w", err)
		}
		var priority int64
		if p, ok := objAttrs.Metadata[priorityMetadataKey]; ok {
			priority, err = strconv.ParseInt(p, 10, 64)
			if err != nil {
				clog.WarnContextf(ctx, "Failed to parse priority: %v", err)
			}
		}
		// Only check for max attempts if this is not a deadlettered item
		if !strings.HasPrefix(objAttrs.Name, deadLetterPrefix) {
			// Check for attempts and track maximum
			if att, ok := objAttrs.Metadata[attemptsMetadataKey]; ok && att != "" {
				attempts, err := strconv.Atoi(att)
				if err != nil {
					clog.WarnContextf(ctx, "Failed to parse attempts: %v", err)
				} else if attempts > maxAttempts {
					maxAttempts = attempts
				}
				if attempts > TrackWorkAttemptMinThreshold {
					l := w.baseLabels()
					l["task_id"] = objAttrs.Name
					mTaskMaxAttempts.With(l).Set(float64(attempts))
				}
			}
		}
		// Ensure metric has a value.
		l := w.baseLabels()
		l["task_id"] = "placeholder"
		mTaskMaxAttempts.With(l).Set(float64(0))

		switch {
		case strings.HasPrefix(objAttrs.Name, inProgressPrefix):
			ipk := &inProgressKey{
				client:    w.client,
				attrs:     objAttrs,
				priority:  priority,
				queueName: w.name,
			}
			wip = append(wip, ipk)

			// Record lease age for active (non-orphaned) keys
			if !ipk.IsOrphaned() {
				leaseAge := time.Since(objAttrs.Created)
				mLeaseAge.With(labels).Observe(leaseAge.Seconds())
			}

		case strings.HasPrefix(objAttrs.Name, queuedPrefix):
			// Calculate time until eligible for all queued keys
			timeUntilEligible := 0.0 // Default: immediately eligible
			if nbf, ok := objAttrs.Metadata[notBeforeMetadataKey]; ok && nbf != "" && nbf != noNotBefore {
				if notBefore, err := time.Parse(time.RFC3339, nbf); err != nil {
					clog.WarnContextf(ctx, "Failed to parse not-before: %v", err)
				} else {
					timeUntilEligible = notBefore.Sub(time.Now().UTC()).Seconds()
					if time.Now().UTC().Before(notBefore) {
						clog.DebugContextf(ctx, "Skipping key %q until %v", objAttrs.Name, notBefore)
						notbefore++
						// Record metric before skipping
						mTimeUntilEligible.With(labels).Observe(timeUntilEligible)
						continue
					}
				}
			}
			// Record metric for immediately eligible keys
			mTimeUntilEligible.With(labels).Observe(timeUntilEligible)

			qd = append(qd, &queuedKey{
				client:    w.client,
				attrs:     objAttrs,
				priority:  priority,
				queueName: w.name,
			})
			sort.Slice(qd, func(i, j int) bool {
				if lhs, rhs := qd[i].Priority(), qd[j].Priority(); lhs != rhs {
					// First consider priority.
					return lhs > rhs
				}
				if !qd[i].attrs.Created.Equal(qd[j].attrs.Created) {
					return qd[i].attrs.Created.Before(qd[j].attrs.Created)
				}
				return qd[i].attrs.Name < qd[j].attrs.Name
			})
			if len(qd) > w.limit {
				qd = qd[:w.limit]
			}
			queued++

		case strings.HasPrefix(objAttrs.Name, deadLetterPrefix):
			// Collect and count the dead-lettered keys
			dl = append(dl, &deadLetteredKey{
				attrs:    objAttrs,
				priority: priority,
			})
			deadlettered++
		}
	}

	qk := make([]workqueue.QueuedKey, 0, len(qd))
	for _, qi := range qd {
		qk = append(qk, qi)
	}

	// Set all metrics
	mInProgressKeys.With(labels).Set(float64(len(wip)))
	mQueuedKeys.With(labels).Set(float64(queued))
	mNotBeforeKeys.With(labels).Set(float64(notbefore))
	mDeadLetteredKeys.With(labels).Set(float64(deadlettered))
	// Set the max attempts metric
	mMaxAttempts.With(labels).Set(float64(maxAttempts))
	return wip, qk, dl, nil
}

type objectAttrs struct {
	attempts    int32
	priority    int64
	notBefore   time.Time
	createdTime time.Time
}

func (w *wq) getAttrs(ctx context.Context, objKey string) (objectAttrs, error) {
	obj := w.client.Object(objKey)
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return objectAttrs{}, err
	}

	var attempts int
	if att, ok := attrs.Metadata[attemptsMetadataKey]; ok && att != "" {
		if parsedAttempts, err := strconv.Atoi(att); err == nil {
			attempts = parsedAttempts
		}
	}

	var priority int64
	if p, ok := attrs.Metadata[priorityMetadataKey]; ok {
		if parsedPriority, err := strconv.ParseInt(p, 10, 64); err == nil {
			priority = parsedPriority
		}
	}

	var notBefore time.Time
	if nb, ok := attrs.Metadata[notBeforeMetadataKey]; ok {
		if parsedTime, err := time.Parse(time.RFC3339, nb); err == nil {
			notBefore = parsedTime
		}
	}

	return objectAttrs{
		attempts:    int32(attempts), //nolint:gosec  // we're not worried about this overflowing
		priority:    priority,
		notBefore:   notBefore,
		createdTime: attrs.Created,
	}, nil
}

func (w *wq) getInProgressKey(ctx context.Context, key string) (*workqueue.KeyState, error) {
	attrs, err := w.getAttrs(ctx, fmt.Sprintf("%s%s", inProgressPrefix, key))
	if err != nil {
		return nil, err
	}
	return &workqueue.KeyState{
		Key:           key,
		Status:        workqueue.KeyState_IN_PROGRESS,
		Attempts:      attrs.attempts,
		Priority:      attrs.priority,
		NotBeforeTime: attrs.notBefore.Unix(),
	}, nil
}

func (w *wq) getQueuedKey(ctx context.Context, key string) (*workqueue.KeyState, error) {
	attrs, err := w.getAttrs(ctx, fmt.Sprintf("%s%s", queuedPrefix, key))
	if err != nil {
		return nil, err
	}
	return &workqueue.KeyState{
		Key:           key,
		Status:        workqueue.KeyState_QUEUED,
		Attempts:      attrs.attempts,
		Priority:      attrs.priority,
		QueuedTime:    attrs.createdTime.Unix(),
		NotBeforeTime: attrs.notBefore.Unix(),
	}, nil
}

func (w *wq) getDeadLetterKey(ctx context.Context, key string) (*workqueue.KeyState, error) {
	attrs, err := w.getAttrs(ctx, fmt.Sprintf("%s%s", deadLetterPrefix, key))
	if err != nil {
		return nil, err
	}
	return &workqueue.KeyState{
		Key:           key,
		Status:        workqueue.KeyState_DEAD_LETTER,
		Attempts:      attrs.attempts,
		Priority:      attrs.priority,
		QueuedTime:    attrs.createdTime.Unix(),
		NotBeforeTime: attrs.notBefore.Unix(),
	}, nil
}

// Get implements workqueue.Interface.
func (w *wq) Get(ctx context.Context, key string) (*workqueue.KeyState, error) {
	if state, err := w.getInProgressKey(ctx, key); err == nil {
		return state, nil
	}

	if state, err := w.getQueuedKey(ctx, key); err == nil {
		return state, nil
	}

	if state, err := w.getDeadLetterKey(ctx, key); err == nil {
		return state, nil
	}

	return nil, status.Errorf(codes.NotFound, "key %q not found", key)
}

type inProgressKey struct {
	client      ClientInterface
	ownerCtx    context.Context
	ownerCancel context.CancelFunc

	// heartbeatStopped is closed when the heartbeat goroutine exits; it is nil
	// for keys observed via Enumerate, which do not heartbeat.
	heartbeatStopped chan struct{}

	priority int64
	// queueName is the queue_name label value applied to all metrics emitted
	// from this key's lifecycle (Requeue, Complete, Deadletter).
	queueName string

	// Once we start to heartbeat things, then that thread may update attrs,
	// so use the RWMutex to protect it from concurrent access.
	rw    sync.RWMutex
	attrs *storage.ObjectAttrs
}

// baseLabels returns the labels every metric emitted from this key carries.
func (o *inProgressKey) baseLabels() prometheus.Labels {
	return prometheus.Labels{
		"service_name":  baseServiceName,
		"revision_name": baseRevisionName,
		"queue_name":    o.queueName,
	}
}

var _ workqueue.ObservedInProgressKey = (*inProgressKey)(nil)
var _ workqueue.OwnedInProgressKey = (*inProgressKey)(nil)

// Name implements workqueue.Key.
func (o *inProgressKey) Name() string {
	o.rw.RLock()
	defer o.rw.RUnlock()
	return strings.TrimPrefix(o.attrs.Name, inProgressPrefix)
}

// Priority implements workqueue.Key.
func (o *inProgressKey) Priority() int64 {
	return o.priority
}

// GetAttempts implements workqueue.OwnedInProgressKey.
func (o *inProgressKey) GetAttempts() int {
	o.rw.RLock()
	defer o.rw.RUnlock()
	return o.attemptsLocked()
}

// attemptsLocked returns the attempt count recorded on the object's metadata.
// Callers must hold o.rw; it must not acquire the lock itself (sync.RWMutex
// blocks new readers once a writer is pending, so a nested RLock can deadlock
// against the heartbeat's write lock).
func (o *inProgressKey) attemptsLocked() int {
	if o.attrs == nil || o.attrs.Metadata == nil {
		return 0
	}

	attempts, err := strconv.Atoi(o.attrs.Metadata[attemptsMetadataKey])
	if err != nil {
		clog.WarnContextf(o.ownerCtx, "Malformed attempts on %s: %v",
			strings.TrimPrefix(o.attrs.Name, inProgressPrefix), err)
		return 0
	}
	return attempts
}

// stopHeartbeat cancels the lease heartbeat and waits for its goroutine to
// exit, so cleanup paths observe a stable attrs and can never race an
// in-flight refresh. It is a no-op for keys observed via Enumerate, which do
// not heartbeat.
func (o *inProgressKey) stopHeartbeat() {
	if o.ownerCancel != nil {
		o.ownerCancel()
	}
	if o.heartbeatStopped != nil {
		<-o.heartbeatStopped
	}
}

// Requeue implements workqueue.InProgressKey.
func (o *inProgressKey) Requeue(ctx context.Context) error {
	// Check if this key is orphaned (lease expired) before requeueing
	if o.IsOrphaned() {
		mExpiredLeases.With(o.baseLabels()).Add(1)
	}
	// Use RequeueWithOptions with an empty options struct to get default behavior
	return o.RequeueWithOptions(ctx, workqueue.Options{})
}

// RequeueWithOptions implements workqueue.InProgressKey.
func (o *inProgressKey) RequeueWithOptions(ctx context.Context, opts workqueue.Options) error {
	o.stopHeartbeat()

	// The delete of the in-progress object is pinned to the generation we
	// leased so we can never remove an object another attempt owns.
	conds := storage.Conditions{GenerationMatch: o.attrs.Generation}

	// A key observed via Enumerate has no heartbeat keeping its view of the
	// lease current: the owner may have refreshed it (which bumps only the
	// metageneration, not the generation) since we listed it. Re-read the
	// object and skip the requeue unless it still carries exactly the lease we
	// observed; pin the delete to that metageneration so a refresh landing
	// after this check is still caught. Owner-held keys deliberately do NOT
	// pin the metageneration: a refresh aborted client-side by cancellation
	// can still land server-side, leaving our recorded metageneration stale
	// for an object we do own.
	if o.ownerCancel == nil {
		attrs, err := o.client.Object(o.attrs.Name).Attrs(ctx)
		switch {
		case isNotFound(err):
			clog.WarnContextf(ctx, "RequeueWithOptions: lost ownership of key %q, skipping requeue: %v", o.Name(), err)
			return nil
		case err != nil:
			return fmt.Errorf("Attrs() = %w", err)
		case attrs.Generation != o.attrs.Generation || attrs.Metageneration != o.attrs.Metageneration:
			clog.WarnContextf(ctx, "RequeueWithOptions: lease on key %q changed since observation, skipping requeue", o.Name())
			return nil
		}
		conds.MetagenerationMatch = attrs.Metageneration
	}

	for {
		retry, err := o.requeueOnce(ctx, opts, conds)
		if !retry {
			return err
		}
	}
}

// requeueOnce performs a single requeue attempt: copy the in-progress object
// back to the queued prefix (or merge into an existing queued twin) and delete
// the in-progress object under deleteConds. It reports retry=true when the
// queued twin vanished mid-merge and the whole sequence should be re-run.
func (o *inProgressKey) requeueOnce(ctx context.Context, opts workqueue.Options, deleteConds storage.Conditions) (bool, error) {
	o.rw.RLock()
	defer o.rw.RUnlock()

	// We'll move from the in-progress to the queued prefix. The copy source is
	// pinned to the generation we leased: if the in-progress object has been
	// replaced by another attempt, we must not copy (or later delete) the new
	// owner's object.
	key := strings.TrimPrefix(o.attrs.Name, inProgressPrefix)
	copier := o.client.Object(fmt.Sprintf("%s%s", queuedPrefix, key)).If(storage.Conditions{
		DoesNotExist: true,
	}).CopierFrom(o.client.Object(o.attrs.Name).Generation(o.attrs.Generation))

	// Preserve metadata
	copier.Metadata = o.attrs.Metadata
	if copier.Metadata == nil {
		copier.Metadata = make(map[string]string)
	}
	// Clear the lease expiration when copying the object back to avoid
	// confusion since the object is no longer in progress.
	delete(copier.Metadata, expirationMetadataKey)
	// Set the last attempted time as unix timestamp when requeuing
	copier.Metadata[lastAttemptedKey] = strconv.FormatInt(time.Now().UTC().Unix(), 10)

	// Handle custom delay if specified
	if opts.BackoffDelay > 0 {
		// Failure-retry backoff: wait BackoffDelay before reprocessing WITHOUT
		// resetting the attempt count and regardless of priority, so the
		// dispatcher's attempts >= maxRetry dead-letter cutoff stays reachable.
		// The caller owns the backoff curve (e.g. decorrelated exponential
		// jitter); this only translates it into a not-before.
		notBefore := time.Now().UTC().Add(opts.BackoffDelay)
		copier.Metadata[notBeforeMetadataKey] = notBefore.Format(time.RFC3339)
	} else if opts.Delay > 0 {
		// Reset attempts when using custom delay, as this indicates periodic revisit pattern
		// rather than retry due to failure
		copier.Metadata[attemptsMetadataKey] = "0"
		notBefore := time.Now().UTC().Add(opts.Delay)
		copier.Metadata[notBeforeMetadataKey] = notBefore.Format(time.RFC3339)
	} else if p, ok := copier.Metadata[priorityMetadataKey]; ok && p != noPriority {
		// If no custom delay and priority is set, use the standard backoff
		attempts, err := strconv.Atoi(copier.Metadata[attemptsMetadataKey])
		if err != nil {
			clog.WarnContextf(ctx, "Malformed attempts on %s: %v", key, err)
			attempts = 1
		}
		backoffDelay := min(time.Duration(attempts*int(workqueue.BackoffPeriod)), workqueue.MaximumBackoffPeriod)
		copier.Metadata[notBeforeMetadataKey] = time.Now().UTC().Add(backoffDelay).Format(time.RFC3339)
	}

	// Apply this requeue's floor intent. Start clears any stale floor from the
	// in-progress object, so we only need to set it (never clear) here.
	if opts.NotBeforeFloor {
		copier.Metadata[notBeforeFloorMetadataKey] = notBeforeFloorValue
	}

	// Update priority if specified
	if opts.Priority != 0 {
		copier.Metadata[priorityMetadataKey] = strconv.FormatInt(opts.Priority, 10)
	}

	_, err := copier.Run(ctx)
	if isNotFound(err) {
		// The source is pinned to the generation we leased, so not-found means
		// that generation is gone: another attempt owns the key now, and
		// requeueing is its responsibility, not ours. Touch nothing.
		clog.WarnContextf(ctx, "RequeueWithOptions: lost ownership of key %q, skipping requeue: %v", key, err)
		return false, nil
	}
	if exists, err := checkPreconditionFailedOk(err); err != nil {
		clog.WarnContextf(ctx, "RequeueWithOptions: copy to queued failed for key %q: %v", key, err)
		return false, fmt.Errorf("Run() = %w", err)
	} else if exists {
		if err := updateMetadata(ctx, o.client, key, copier.Metadata); err != nil {
			if errors.Is(err, storage.ErrObjectNotExist) {
				clog.InfoContextf(ctx, "Key %q was deleted before we could fetch the duplicate, recursing.", key)
				return true, nil
			}
			return false, fmt.Errorf("updateMetadata() = %w", err)
		}
	}
	if err := deleteWithRetry(ctx, o.client.Object(o.attrs.Name).If(deleteConds)); err != nil {
		if lostOwnership(err) {
			// The owner refreshed the lease (or another attempt replaced the
			// object, or it is already gone) between our copy and this pinned
			// delete, so leave the queued twin in place. We must NOT delete it: a
			// concurrent enqueue may have deduplicated into the twin without
			// bumping its metageneration (dedups are deliberately cheap no-ops),
			// so a pristine twin is indistinguishable from one now carrying a real
			// queued event, and deleting it could drop that event. A spurious
			// re-execution (the twin runs once the live attempt completes) is the
			// safe bias: workqueue receivers are idempotent, so at-least-once
			// reprocessing is acceptable where dropping a key is not.
			clog.WarnContextf(ctx, "RequeueWithOptions: lost ownership of key %q, leaving requeue twin intact: %v", key, err)
			return false, nil
		}
		clog.WarnContextf(ctx, "RequeueWithOptions: failed to delete in-progress object for key %q: %v", key, err)
		return false, err
	}
	return false, nil
}

// IsOrphaned implements workqueue.ObservedInProgressKey.
func (o *inProgressKey) IsOrphaned() bool {
	o.rw.RLock()
	defer o.rw.RUnlock()

	exp, ok := o.attrs.Metadata[expirationMetadataKey]
	if !ok {
		// No expiration metadata should be treated as orphaned.
		return true
	}
	expiry, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		// Malformed expiration metadata should be treated as orphaned.
		return true
	}

	// If the expiration time is in the past, then this key is orphaned.
	return time.Now().UTC().After(expiry)
}

// deadLetterKey returns the dead letter key for this in-progress key
func (o *inProgressKey) deadLetterKey() string {
	key := strings.TrimPrefix(o.attrs.Name, inProgressPrefix)
	return fmt.Sprintf("%s%s", deadLetterPrefix, key)
}

// deleteWithRetry deletes a GCS object, retrying with jittered exponential
// backoff on transient errors (e.g., 429 rate limiting). It retries up to
// 10 times before giving up.
func deleteWithRetry(ctx context.Context, obj *storage.ObjectHandle) error {
	baseDelay := 500 * time.Millisecond
	maxDelay := 30 * time.Second

	for attempt := range 10 { // upper bound to prevent infinite loop
		err := obj.Delete(ctx)
		if err == nil {
			return nil
		}

		// Only retry on rate limit (429) errors.
		var gerr *googleapi.Error
		if !errors.As(err, &gerr) || gerr.Code != http.StatusTooManyRequests {
			return err
		}

		// Jittered exponential backoff: base * 2^attempt, capped at maxDelay.
		delay := min(baseDelay<<attempt, maxDelay)
		jitter := time.Duration(rand.Int64N(int64(delay))) //nolint:gosec // Weak random is fine for jitter
		sleep := delay + jitter

		clog.WarnContextf(ctx, "deleteWithRetry: 429 on attempt %d, retrying in %v: %v", attempt+1, sleep, err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
	}

	return obj.Delete(ctx) // final attempt
}

// Complete implements workqueue.OwnedInProgressKey.
func (o *inProgressKey) Complete(ctx context.Context) error {
	o.stopHeartbeat()
	o.rw.RLock()
	defer o.rw.RUnlock()

	workLabels := o.baseLabels()
	workLabels["priority_class"] = priorityClass(o.priority)
	mWorkLatency.With(workLabels).Observe(time.Now().UTC().Sub(o.attrs.Created).Seconds())

	// Record the number of attempts for this successful completion
	attempts := o.attemptsLocked()
	mCompletionAttempts.With(o.baseLabels()).Observe(float64(attempts))

	// Record time to completion
	ttcLabels := o.baseLabels()
	ttcLabels["priority_class"] = priorityClass(o.priority)
	ttcLabels["status"] = "success"
	mTimeToCompletion.With(ttcLabels).Observe(time.Now().UTC().Sub(o.attrs.Created).Seconds())

	// Best-effort delete of the dead-letter object, if it exists.
	if err := o.client.Object(o.deadLetterKey()).Delete(ctx); err != nil {
		if !errors.Is(err, storage.ErrObjectNotExist) {
			clog.WarnContextf(ctx, "Complete: failed to delete dead-letter object for key %q: %v",
				strings.TrimPrefix(o.attrs.Name, inProgressPrefix), err)
		}
	}

	if err := deleteWithRetry(ctx, o.client.Object(o.attrs.Name).If(storage.Conditions{
		GenerationMatch: o.attrs.Generation,
	})); err != nil {
		if lostOwnership(err) {
			// The work completed, but another attempt owns the key now; its
			// lease object must be left intact.
			clog.WarnContextf(ctx, "Complete: lost ownership of key %q, skipping delete: %v",
				strings.TrimPrefix(o.attrs.Name, inProgressPrefix), err)
			return nil
		}
		clog.ErrorContextf(ctx, "Complete: failed to delete in-progress object for key %q: %v",
			strings.TrimPrefix(o.attrs.Name, inProgressPrefix), err)
		return err
	}
	return nil
}

// Deadletter implements workqueue.OwnedInProgressKey.
func (o *inProgressKey) Deadletter(ctx context.Context) error {
	o.stopHeartbeat()
	o.rw.RLock()
	defer o.rw.RUnlock()

	key := strings.TrimPrefix(o.attrs.Name, inProgressPrefix)
	deadLetterKey := o.deadLetterKey()

	clog.InfoContextf(ctx, "Moving key %q to dead letter queue as %q", key, deadLetterKey)

	// Copy the in-progress task to the dead letter queue. The copy source is
	// pinned to the generation we leased so that a replacement by another
	// attempt is observed as loss of ownership rather than dead-lettering the
	// new owner's object.
	copier := o.client.Object(deadLetterKey).CopierFrom(o.client.Object(o.attrs.Name).Generation(o.attrs.Generation))

	// Preserve metadata
	copier.Metadata = o.attrs.Metadata
	if copier.Metadata == nil {
		copier.Metadata = make(map[string]string)
	}

	// Clear the lease expiration when copying the object
	delete(copier.Metadata, expirationMetadataKey)

	// Add metadata about when the key was dead-lettered
	copier.Metadata[failedTimeMetadataKey] = time.Now().UTC().Format(time.RFC3339)

	// Record time to completion for dead-lettered task
	ttcLabels := o.baseLabels()
	ttcLabels["priority_class"] = priorityClass(o.priority)
	ttcLabels["status"] = "dead-lettered"
	mTimeToCompletion.With(ttcLabels).Observe(time.Now().UTC().Sub(o.attrs.Created).Seconds())

	// Create the dead letter entry
	_, err := copier.Run(ctx)
	if isNotFound(err) {
		// The source is pinned to the generation we leased, so not-found means
		// that generation is gone: another attempt owns the key now. Leave its
		// state alone rather than dead-lettering it.
		clog.WarnContextf(ctx, "Deadletter: lost ownership of key %q, skipping dead-letter: %v", key, err)
		return nil
	}
	if err != nil {
		clog.WarnContextf(ctx, "Deadletter: copy to dead-letter failed for key %q: %v", key, err)
		return fmt.Errorf("failed to create dead letter entry: %w", err)
	}

	// Delete the in-progress task
	if err := deleteWithRetry(ctx, o.client.Object(o.attrs.Name).If(storage.Conditions{
		GenerationMatch: o.attrs.Generation,
	})); err != nil {
		if lostOwnership(err) {
			// Another attempt replaced the object between our copy and delete;
			// the new owner's lease object must be left intact.
			clog.WarnContextf(ctx, "Deadletter: lost ownership of key %q, skipping delete: %v", key, err)
			return nil
		}
		clog.WarnContextf(ctx, "Deadletter: failed to delete in-progress object for key %q: %v", key, err)
		return err
	}
	return nil
}

// Context implements workqueue.OwnedInProgressKey.
func (o *inProgressKey) Context() context.Context {
	return o.ownerCtx
}

func (o *inProgressKey) startHeartbeat(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	o.ownerCtx = ctx
	o.ownerCancel = cancel
	o.heartbeatStopped = make(chan struct{})

	go func() {
		defer close(o.heartbeatStopped)
		ticker := time.NewTicker(RefreshInterval)
		defer ticker.Stop()
		defer cancel()

		// Start stamped the object with a lease that expires 3x the refresh
		// interval from creation; each successful refresh pushes it out again.
		// Prefer the stamped expiration over recomputing it, so we never
		// believe the lease outlives what other dispatchers can observe.
		expiry := time.Now().UTC().Add(3 * RefreshInterval)
		if exp, err := time.Parse(time.RFC3339, o.attrs.Metadata[expirationMetadataKey]); err == nil {
			expiry = exp
		}

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				newExpiry, ok := o.refreshLease(ctx, expiry)
				if !ok {
					return
				}
				expiry = newExpiry
			}
		}
	}()
}

// refreshLease extends the lease on the in-progress object, retrying transient
// errors until the current lease expires. It returns the new expiration and
// whether ownership is retained; on a definitive loss of ownership (the object
// was replaced or deleted out from under us) or once the lease can no longer
// be extended before it lapses, it returns false so the caller cancels the
// in-progress work.
func (o *inProgressKey) refreshLease(ctx context.Context, expiry time.Time) (time.Time, bool) {
	// Clamp the retry pacing to the refresh interval so that shortening
	// RefreshInterval (a public knob) cannot leave the lease with no retry
	// budget before it lapses.
	retryInterval := min(heartbeatRetryInterval, RefreshInterval)

	for {
		// A canceled owner (cleanup underway) must not contend for the write
		// lock or log lease errors that are byproducts of shutdown.
		if ctx.Err() != nil {
			return time.Time{}, false
		}

		newExpiry := time.Now().UTC().Add(3 * RefreshInterval)

		// The function invocation is to scope the defers, and the lock is
		// released between retries so cleanup paths are not blocked while
		// we wait out a transient storage error. The per-attempt timeout
		// bounds the storage client's internal retrying so that a persistent
		// outage cannot pin a single attempt past the lease expiry.
		err := func() error {
			uctx, ucancel := context.WithTimeout(ctx, retryInterval)
			defer ucancel()

			o.rw.Lock()
			defer o.rw.Unlock()

			attrs, err := o.client.Object(o.attrs.Name).If(storage.Conditions{
				// Pin the generation we leased: if the object is replaced by
				// another attempt it gets a NEW generation, so the refresh can
				// never adopt someone else's lease (a metageneration-only pin
				// could: fresh generations restart the metageneration counter).
				// The metageneration is deliberately NOT pinned. Only the owner
				// ever updates an in-progress generation's metadata, and one of
				// our own refreshes can land server-side while failing
				// client-side (timeout, lost response), leaving our recorded
				// metageneration stale; pinning it would 412 against our own
				// write and cancel healthy work.
				GenerationMatch: o.attrs.Generation,
			}).Update(uctx, storage.ObjectAttrsToUpdate{
				Metadata: map[string]string{
					expirationMetadataKey: newExpiry.Format(time.RFC3339),
				},
			})
			if err != nil {
				return err
			}
			// This is what we're guarding with the write lock.
			o.attrs = attrs
			return nil
		}()
		switch {
		case err == nil:
			return newExpiry, true

		case ctx.Err() != nil:
			// Shutting down: the failure is a byproduct of cancellation, not
			// a lease problem.
			return time.Time{}, false

		case lostOwnership(err):
			clog.ErrorContextf(ctx, "refreshLease: lost ownership of %q, terminating in-progress work: %v", o.Name(), err)
			return time.Time{}, false
		}

		// A transient error (rate limiting, server error, network blip) does
		// not mean we lost ownership: the object still carries our lease. Keep
		// retrying until the lease would lapse and others may treat the key as
		// orphaned; only then give up.
		if !time.Now().UTC().Add(retryInterval).Before(expiry) {
			clog.ErrorContextf(ctx, "refreshLease: failed to update expiration for %q before lease expiry, terminating in-progress work: %v", o.Name(), err)
			return time.Time{}, false
		}
		clog.WarnContextf(ctx, "refreshLease: failed to update expiration for %q (will retry): %v", o.Name(), err)

		select {
		case <-ctx.Done():
			return time.Time{}, false
		case <-time.After(retryInterval):
		}
	}
}

type queuedKey struct {
	client   ClientInterface
	attrs    *storage.ObjectAttrs
	priority int64
	// queueName is the queue_name label value applied to all metrics emitted
	// when this key is started.
	queueName string
}

// baseLabels returns the labels every metric emitted from this key carries.
func (q *queuedKey) baseLabels() prometheus.Labels {
	return prometheus.Labels{
		"service_name":  baseServiceName,
		"revision_name": baseRevisionName,
		"queue_name":    q.queueName,
	}
}

var _ workqueue.QueuedKey = (*queuedKey)(nil)

// Name implements workqueue.Key.
func (q *queuedKey) Name() string {
	return strings.TrimPrefix(q.attrs.Name, queuedPrefix)
}

// Priority implements workqueue.Key.
func (q *queuedKey) Priority() int64 {
	return q.priority
}

// Start implements workqueue.QueuedKey.
func (q *queuedKey) Start(ctx context.Context) (workqueue.OwnedInProgressKey, error) {
	// We'll move from the in-progress to the queued prefix.
	srcObject := q.attrs.Name
	key := strings.TrimPrefix(srcObject, queuedPrefix)
	targetObject := fmt.Sprintf("%s%s", inProgressPrefix, key)

	// Calculate wait latency using last attempted time if available, otherwise use created time
	waitStart := q.attrs.Created
	if lastAttempted, ok := q.attrs.Metadata[lastAttemptedKey]; ok && lastAttempted != "" {
		if lastAttemptedUnix, err := strconv.ParseInt(lastAttempted, 10, 64); err == nil {
			waitStart = time.Unix(lastAttemptedUnix, 0).UTC()
		}
	}
	waitLabels := q.baseLabels()
	waitLabels["priority_class"] = priorityClass(q.priority)
	mWaitLatency.With(waitLabels).Observe(time.Now().UTC().Sub(waitStart).Seconds())

	// Calculate wait latency from scheduled time.
	scheduledStart := waitStart
	if notBefore, ok := q.attrs.Metadata[notBeforeMetadataKey]; ok && notBefore != "" && notBefore != noNotBefore {
		if notBeforeTime, err := time.Parse(time.RFC3339, notBefore); err == nil {
			scheduledStart = notBeforeTime
		}
	}
	schedLabels := q.baseLabels()
	schedLabels["priority_class"] = priorityClass(q.priority)
	mWaitLatencyFromScheduled.With(schedLabels).Observe(time.Now().UTC().Sub(scheduledStart).Seconds())

	// Create a copier to copy the source object, and then we will delete it
	// upon success.
	copier := q.client.Object(targetObject).If(storage.Conditions{
		DoesNotExist: true,
	}).CopierFrom(q.client.Object(srcObject))

	// Preserve metadata
	copier.Metadata = q.attrs.Metadata
	if copier.Metadata == nil {
		copier.Metadata = make(map[string]string, 2)
	}
	// Set the expiration metadata to 3x the refresh interval.
	copier.Metadata[expirationMetadataKey] = time.Now().UTC().Add(3 * RefreshInterval).Format(time.RFC3339)
	if att, ok := copier.Metadata[attemptsMetadataKey]; ok {
		prevAttempts, err := strconv.Atoi(att)
		if err != nil {
			clog.ErrorContextf(ctx, "Malformed attempts on %s: %v", srcObject, err)
			copier.Metadata[attemptsMetadataKey] = "1"
		} else {
			copier.Metadata[attemptsMetadataKey] = fmt.Sprint(prevAttempts + 1)
		}
	} else {
		copier.Metadata[attemptsMetadataKey] = "1"
	}
	// Never persist the not-before metadata to a running task.
	// We set it to the zero value instead of deleting it so that we can assume
	// the invariant that this key is always present and date-formatted.
	copier.Metadata[notBeforeMetadataKey] = noNotBefore
	// The not-before floor only applies to queued entries; clear it so it does
	// not leak onto the in-progress object (or, via Deadletter's verbatim copy,
	// onto dead-letter objects). RequeueWithOptions re-applies it on request.
	delete(copier.Metadata, notBeforeFloorMetadataKey)

	attrs, err := copier.Run(ctx)
	if err != nil {
		clog.WarnContextf(ctx, "Start: copy to in-progress failed for key %q: %v", key, err)
		return nil, fmt.Errorf("Run() = %w", err)
	}
	if err := q.client.Object(srcObject).Delete(ctx); err != nil {
		// The in-progress object was already created by the copy above,
		// so we proceed normally. The queued object will be cleaned up
		// on the next Enumerate when Start is called again, which will
		// fail the copy (DoesNotExist precondition) and the key will be
		// processed a second time as a result.
		clog.WarnContextf(ctx, "Failed to delete queued object %q after starting: %v (key will be processed multiple times)", srcObject, err)
	}

	oip := &inProgressKey{
		client:    q.client,
		attrs:     attrs,
		priority:  q.priority,
		queueName: q.queueName,
	}

	// start a process to heartbeat things, and set up a context that we can
	// cancel if the heartbeat observes a loss in ownership.
	oip.startHeartbeat(ctx)

	return oip, nil
}

type deadLetteredKey struct {
	attrs    *storage.ObjectAttrs
	priority int64
}

var _ workqueue.DeadLetteredKey = (*deadLetteredKey)(nil)

// Name implements workqueue.Key.
func (d *deadLetteredKey) Name() string {
	return strings.TrimPrefix(d.attrs.Name, deadLetterPrefix)
}

// Priority implements workqueue.Key.
func (d *deadLetteredKey) Priority() int64 {
	return d.priority
}

// GetFailedTime implements workqueue.DeadLetteredKey.
func (d *deadLetteredKey) GetFailedTime() time.Time {
	if ft, ok := d.attrs.Metadata[failedTimeMetadataKey]; ok && ft != "" {
		if failedTime, err := time.Parse(time.RFC3339, ft); err == nil {
			return failedTime
		}
	}
	// Fall back to the object creation time if failed-time metadata is not available
	return d.attrs.Created
}

// GetAttempts implements workqueue.DeadLetteredKey.
func (d *deadLetteredKey) GetAttempts() int {
	if att, ok := d.attrs.Metadata[attemptsMetadataKey]; ok && att != "" {
		if attempts, err := strconv.Atoi(att); err == nil {
			return attempts
		}
	}
	return 0
}
