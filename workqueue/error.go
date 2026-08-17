/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NoRetryDetails marks the error as non-retriable with the given reason.
// If this error is returned to the dispatcher, it will not requeue the key.
func NonRetriableError(err error, reason string) error {
	// NonRetriableError is a marker error that indicates that the key should not be retried.
	if err == nil {
		// No error, nothing to do.
		return nil
	}

	// We ignore ok - usually happens when the error is not a gRPC error.
	s, _ := status.FromError(err)
	s, derr := s.WithDetails(&NoRetryDetails{
		Message: reason,
	})
	if derr != nil {
		// This shouldn't generally happen since this should only happen if the details aren't a protobuf message,
		// but if it does, we return the original error.
		return err
	}

	return s.Err()
}

// GetNonRetriableDetails extracts the NoRetryDetails from the error if it exists.
// If the error is nil or does not contain NoRetryDetails, it returns nil.
func GetNonRetriableDetails(err error) *NoRetryDetails {
	if err == nil {
		return nil
	}

	s, ok := status.FromError(err)
	if !ok {
		return nil
	}

	for _, detail := range s.Details() {
		if nrd, ok := detail.(*NoRetryDetails); ok {
			return nrd
		}
	}
	return nil
}

// IsInfrastructureError reports whether err is an infrastructure failure and
// not an application verdict. An error is one of two forms: InfrastructureError
// marked it, or it carries codes.Unavailable. gRPC transports set
// codes.Unavailable when the receiver was not reachable or the connection broke
// mid-call (for example, the receiving instance was killed), and reconcilers
// propagate codes.Unavailable when a downstream dependency is unavailable. The
// classification does not change scheduling, because every retriable failure
// requeues on the same widening backoff curve. The classification is for
// observability (dispatch error events and logs), where the split between
// infrastructure churn and application failures makes the failure-rate
// dashboards readable.
func IsInfrastructureError(err error) bool {
	var ie *infrastructureError
	if errors.As(err, &ie) {
		return true
	}
	return status.Code(err) == codes.Unavailable
}

// infrastructureError marks a wrapped error as an infrastructure failure. The
// marker is transparent: its message is the wrapped error's message, and it
// unwraps to the wrapped error and to the causes, so type matching and sentinel
// matching against either one still succeed.
type infrastructureError struct {
	err    error
	causes []error
}

// Error implements the error interface.
func (e *infrastructureError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the wrapped error and the causes to errors.Is and errors.As.
func (e *infrastructureError) Unwrap() []error {
	unwrapped := make([]error, 0, 1+len(e.causes))
	unwrapped = append(unwrapped, e.err)
	return append(unwrapped, e.causes...)
}

// InfrastructureError marks err as an infrastructure failure, so that
// IsInfrastructureError reports it as one whatever gRPC code err carries. Use
// InfrastructureError when only the call site can recognise a provider failure,
// for example when it wraps a requeue error returned for a transport fault:
//
//	InfrastructureError(RequeueAfter(5*time.Minute), cause)
//
// The marker does not change the wrapped error's message and does not hide the
// wrapped error. The wrapped error and every cause stay reachable through
// errors.Is and errors.As, so GetRequeueOptions still finds the requeue delay
// and callers still match the original provider cause. Match the marker with
// IsInfrastructureError, never on message text. InfrastructureError returns nil
// when err is nil.
func InfrastructureError(err error, causes ...error) error {
	if err == nil {
		return nil
	}
	return &infrastructureError{err: err, causes: causes}
}

// HasInfrastructureMarker reports whether err carries the marker that
// InfrastructureError put on it. It is narrower than IsInfrastructureError,
// which also reports true for every error carrying codes.Unavailable:
// HasInfrastructureMarker matches the marker alone and never the status-code
// class. Use it where only a deliberate classification at the call site counts,
// for example where a scorer exempts a provider outage but a bare
// codes.Unavailable from the run's own work must still count against it. The
// marker is matched by type, never by message text.
func HasInfrastructureMarker(err error) bool {
	var ie *infrastructureError
	return errors.As(err, &ie)
}

// InfrastructureCauses returns the causes InfrastructureError recorded on err,
// or nil when err carries no marker or when the marker carries no cause. The
// marker's message is the wrapped error's message, so a caller that reports the
// failure in text (a log line, an eval score reasoning) needs the causes to name
// the provider failure behind it. Nil causes are dropped, because a nil cause
// names nothing. The marker is matched by type, never by message text.
func InfrastructureCauses(err error) []error {
	var ie *infrastructureError
	if !errors.As(err, &ie) {
		return nil
	}
	causes := make([]error, 0, len(ie.causes))
	for _, cause := range ie.causes {
		if cause != nil {
			causes = append(causes, cause)
		}
	}
	if len(causes) == 0 {
		return nil
	}
	return causes
}

// requeueError is a special error type that indicates the work item should be
// requeued with a specific delay.
type requeueError struct {
	delay time.Duration
	// floor, when true, marks the requeue NotBefore as a floor that subsequent
	// enqueues of the same key cannot pull earlier (see RequeueNotBefore).
	floor bool
}

// Error implements the error interface.
func (e *requeueError) Error() string {
	return fmt.Sprintf("requeue after %v (floor=%t)", e.delay, e.floor)
}

// RequeueAfter returns an error that indicates the work item should be requeued
// after the specified delay. The resulting NotBefore is undercuttable: a fresh
// enqueue of the same key (e.g. from a new event) before the delay elapses will
// pull the key forward and process it immediately. Use this for retry/backoff
// where promptly reacting to new events is desirable.
func RequeueAfter(delay time.Duration) error {
	return &requeueError{delay: delay}
}

// RequeueAfterWithJitter is like RequeueAfter, but adds a random extra delay
// in [0, jitter) so keys that failed together don't all come back at once.
func RequeueAfterWithJitter(delay, jitter time.Duration) error {
	if jitter > 0 {
		delay += rand.N(jitter) //nolint:gosec // G404: jitter, not security-sensitive
	}
	return RequeueAfter(delay)
}

// RequeueNotBefore is like RequeueAfter, but the resulting NotBefore is a floor:
// subsequent enqueues of the same key that arrive before the delay elapses are
// coalesced onto it rather than pulling the key forward. This debounces a burst
// of events for a key the reconciler has decided to revisit at a fixed cadence
// (e.g. polling for CI to settle) into roughly one reconcile per delay, while
// still observing the latest state when the key fires.
func RequeueNotBefore(delay time.Duration) error {
	return &requeueError{delay: delay, floor: true}
}

// GetRequeueDelay extracts the requeue delay from an error if it's a requeue error.
// Returns the delay and true if the error is a requeue error, or 0 and false otherwise.
// It is a convenience wrapper over GetRequeueOptions for callers that don't need the
// floor flag.
func GetRequeueDelay(err error) (time.Duration, bool) {
	delay, _, ok := GetRequeueOptions(err)
	return delay, ok
}

// GetRequeueOptions extracts the requeue delay and whether its NotBefore should
// be treated as a floor from an error, if it is a requeue error (RequeueAfter or
// RequeueNotBefore). floor is true only for RequeueNotBefore; ok is false for
// non-requeue errors.
func GetRequeueOptions(err error) (delay time.Duration, floor bool, ok bool) {
	var re *requeueError
	if errors.As(err, &re) {
		return re.delay, re.floor, true
	}
	return 0, false, false
}

// QueueKey represents a key to be queued with optional priority and delay.
type QueueKey struct {
	Key          string
	Priority     int64
	DelaySeconds int64
}

// queueKeysError wraps keys that should be queued after successful processing.
type queueKeysError struct {
	keys []QueueKey
}

func (e *queueKeysError) Error() string {
	return fmt.Sprintf("queue %d keys", len(e.keys))
}

// QueueKeys returns a sentinel error indicating additional keys should be queued.
// This is used by reconcilers to signal that dependent work should be enqueued.
// The current key will be completed after all queued keys are successfully added.
// If the current key is included in the list, it will be requeued (enters "dual state").
func QueueKeys(keys ...QueueKey) error {
	if len(keys) == 0 {
		return nil
	}
	return &queueKeysError{keys: keys}
}

// GetQueueKeys extracts queued keys from an error, if present.
// Returns nil if the error doesn't contain queue keys.
func GetQueueKeys(err error) []QueueKey {
	if err == nil {
		return nil
	}
	var qke *queueKeysError
	if errors.As(err, &qke) {
		return qke.keys
	}
	return nil
}
