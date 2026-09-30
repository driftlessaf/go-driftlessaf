/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/google/go-cmp/cmp"
)

func TestLocalAndServiceCallbacksPreserveResponses(t *testing.T) {
	failure := errors.New("temporary failure")
	for _, tc := range []struct {
		name     string
		response *workqueue.ProcessResponse
		failure  error
		delay    time.Duration
		floor    bool
		keys     []workqueue.QueueKey
	}{
		{name: "complete", response: &workqueue.ProcessResponse{}},
		{name: "retry", failure: failure},
		{name: "delay", response: &workqueue.ProcessResponse{RequeueAfterSeconds: 30}, delay: 30 * time.Second},
		{name: "floor", response: &workqueue.ProcessResponse{RequeueAfterSeconds: 60, RequeueFloor: true}, delay: time.Minute, floor: true},
		{name: "dependent keys", response: &workqueue.ProcessResponse{QueueKeys: []*workqueue.QueueKeyRequest{{Key: "next", Priority: 4, DelaySeconds: 90}}}, keys: []workqueue.QueueKey{{Key: "next", Priority: 4, DelaySeconds: 90}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			process := func(_ context.Context, req *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
				if req.GetKey() != "key" || req.GetPriority() != 3 {
					t.Errorf("request = %v", req)
				}
				return tc.response, tc.failure
			}
			for _, callback := range []Callback{ProcessCallback(process), ServiceCallback(&mockWorkqueueClient{processFunc: process})} {
				err := callback(t.Context(), "key", workqueue.Options{Priority: 3})
				if tc.failure != nil && !errors.Is(err, tc.failure) {
					t.Fatalf("error = %v, want %v", err, tc.failure)
				}
				delay, floor, ok := workqueue.GetRequeueOptions(err)
				if delay != tc.delay || floor != tc.floor || ok != (tc.delay > 0) {
					t.Errorf("requeue = %v, %t, %t", delay, floor, ok)
				}
				if diff := cmp.Diff(tc.keys, workqueue.GetQueueKeys(err)); diff != "" {
					t.Errorf("keys mismatch (-want +got):\n%s", diff)
				}
				if tc.failure == nil && tc.delay == 0 && len(tc.keys) == 0 && err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
