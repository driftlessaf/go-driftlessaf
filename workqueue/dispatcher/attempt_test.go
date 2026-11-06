/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"fmt"
	"testing"

	"chainguard.dev/driftlessaf/workqueue"
)

// TestProcessCallbackForwardsAttempt proves the processor sees the attempt's
// number and the retry budget, so it can tell its last attempt from one that
// will be retried.
func TestProcessCallbackForwardsAttempt(t *testing.T) {
	const maxRetry = 5
	for _, attempts := range []int{1, maxRetry} {
		t.Run(fmt.Sprintf("attempt %d", attempts), func(t *testing.T) {
			q := &mockQueue{next: []workqueue.QueuedKey{&mockKey{name: "key", attempts: attempts}}}
			var got *workqueue.ProcessRequest
			cb := ProcessCallback(func(_ context.Context, req *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
				got = req
				return &workqueue.ProcessResponse{}, nil
			})
			if err := HandleAsync(t.Context(), q, 1, 0, cb, maxRetry)(); err != nil {
				t.Fatalf("HandleAsync: %v", err)
			}
			if got == nil {
				t.Fatal("process: not called")
			}
			if got.GetAttempts() != int32(attempts) || got.GetMaxAttempts() != maxRetry {
				t.Errorf("request: got attempts = %d of %d, want = %d of %d", got.GetAttempts(), got.GetMaxAttempts(), attempts, maxRetry)
			}
		})
	}
}
