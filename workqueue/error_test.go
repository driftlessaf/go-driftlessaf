/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRequeueAfter(t *testing.T) {
	tests := []struct {
		name      string
		delay     time.Duration
		wantDelay time.Duration
	}{
		{
			name:      "5 second delay",
			delay:     5 * time.Second,
			wantDelay: 5 * time.Second,
		},
		{
			name:      "1 minute delay",
			delay:     time.Minute,
			wantDelay: time.Minute,
		},
		{
			name:      "zero delay",
			delay:     0,
			wantDelay: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RequeueAfter(tt.delay)
			if err == nil {
				t.Fatal("Expected non-nil error")
			}

			gotDelay, ok := GetRequeueDelay(err)
			if !ok {
				t.Fatal("GetRequeueDelay returned false")
			}
			if gotDelay != tt.wantDelay {
				t.Errorf("Got delay %v, want %v", gotDelay, tt.wantDelay)
			}
		})
	}
}

func TestGetRequeueDelay(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantDelay time.Duration
		wantOk    bool
	}{
		{
			name:      "requeue error",
			err:       RequeueAfter(10 * time.Second),
			wantDelay: 10 * time.Second,
			wantOk:    true,
		},
		{
			name:      "regular error",
			err:       errors.New("some error"),
			wantDelay: 0,
			wantOk:    false,
		},
		{
			name:      "nil error",
			err:       nil,
			wantDelay: 0,
			wantOk:    false,
		},
		{
			name:      "wrapped requeue error",
			err:       fmt.Errorf("operation failed: %w", RequeueAfter(15*time.Second)),
			wantDelay: 15 * time.Second,
			wantOk:    true,
		},
		{
			name:      "double wrapped requeue error",
			err:       fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", RequeueAfter(20*time.Second))),
			wantDelay: 20 * time.Second,
			wantOk:    true,
		},
		{
			name:      "wrapped regular error",
			err:       fmt.Errorf("wrapped: %w", errors.New("some error")),
			wantDelay: 0,
			wantOk:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDelay, gotOk := GetRequeueDelay(tt.err)
			if gotOk != tt.wantOk {
				t.Errorf("Got ok=%v, want %v", gotOk, tt.wantOk)
			}
			if gotDelay != tt.wantDelay {
				t.Errorf("Got delay %v, want %v", gotDelay, tt.wantDelay)
			}
		})
	}
}

func TestGetRequeueOptions(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantDelay time.Duration
		wantFloor bool
		wantOk    bool
	}{
		{name: "requeue not before", err: RequeueNotBefore(10 * time.Second), wantDelay: 10 * time.Second, wantFloor: true, wantOk: true},
		{name: "requeue after is not a floor", err: RequeueAfter(10 * time.Second), wantDelay: 10 * time.Second, wantFloor: false, wantOk: true},
		{name: "wrapped requeue not before", err: fmt.Errorf("outer: %w", RequeueNotBefore(time.Second)), wantDelay: time.Second, wantFloor: true, wantOk: true},
		{name: "regular error", err: errors.New("some error"), wantOk: false},
		{name: "nil error", err: nil, wantOk: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delay, floor, ok := GetRequeueOptions(tt.err)
			if ok != tt.wantOk {
				t.Errorf("ok = %v, want %v", ok, tt.wantOk)
			}
			if delay != tt.wantDelay {
				t.Errorf("delay = %v, want %v", delay, tt.wantDelay)
			}
			if floor != tt.wantFloor {
				t.Errorf("floor = %v, want %v", floor, tt.wantFloor)
			}
		})
	}
}

func TestQueueKeys(t *testing.T) {
	tests := []struct {
		name     string
		keys     []QueueKey
		wantNil  bool
		wantKeys []QueueKey
	}{{
		name:    "no keys returns nil",
		keys:    nil,
		wantNil: true,
	}, {
		name:    "empty slice returns nil",
		keys:    []QueueKey{},
		wantNil: true,
	}, {
		name: "single key",
		keys: []QueueKey{{
			Key: "key1",
		}},
		wantKeys: []QueueKey{{
			Key: "key1",
		}},
	}, {
		name: "multiple keys",
		keys: []QueueKey{{
			Key: "key1",
		}, {
			Key:      "key2",
			Priority: 100,
		}, {
			Key:          "key3",
			DelaySeconds: 60,
		}},
		wantKeys: []QueueKey{{
			Key: "key1",
		}, {
			Key:      "key2",
			Priority: 100,
		}, {
			Key:          "key3",
			DelaySeconds: 60,
		}},
	}, {
		name: "key with all fields",
		keys: []QueueKey{{
			Key:          "full-key",
			Priority:     500,
			DelaySeconds: 120,
		}},
		wantKeys: []QueueKey{{
			Key:          "full-key",
			Priority:     500,
			DelaySeconds: 120,
		}},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := QueueKeys(tt.keys...)
			if tt.wantNil {
				if err != nil {
					t.Errorf("QueueKeys() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("QueueKeys() = nil, want non-nil error")
			}
			gotKeys := GetQueueKeys(err)
			if diff := cmp.Diff(tt.wantKeys, gotKeys); diff != "" {
				t.Errorf("GetQueueKeys() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGetQueueKeys(t *testing.T) {
	testKeys := []QueueKey{{
		Key:      "test-key",
		Priority: 50,
	}}

	tests := []struct {
		name     string
		err      error
		wantKeys []QueueKey
	}{{
		name:     "queue keys error",
		err:      QueueKeys(testKeys...),
		wantKeys: testKeys,
	}, {
		name:     "regular error",
		err:      errors.New("some error"),
		wantKeys: nil,
	}, {
		name:     "nil error",
		err:      nil,
		wantKeys: nil,
	}, {
		name:     "wrapped queue keys error",
		err:      fmt.Errorf("operation failed: %w", QueueKeys(testKeys...)),
		wantKeys: testKeys,
	}, {
		name: "double wrapped queue keys error",
		err: fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", QueueKeys(
			QueueKey{Key: "deep-key", Priority: 200},
		))),
		wantKeys: []QueueKey{{Key: "deep-key", Priority: 200}},
	}, {
		name:     "wrapped regular error",
		err:      fmt.Errorf("wrapped: %w", errors.New("some error")),
		wantKeys: nil,
	}, {
		name:     "requeue error (not queue keys)",
		err:      RequeueAfter(5 * time.Second),
		wantKeys: nil,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKeys := GetQueueKeys(tt.err)
			if diff := cmp.Diff(tt.wantKeys, gotKeys); diff != "" {
				t.Errorf("GetQueueKeys() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestQueueKeysErrorMessage(t *testing.T) {
	tests := []struct {
		name    string
		keys    []QueueKey
		wantMsg string
	}{{
		name:    "single key",
		keys:    []QueueKey{{Key: "key1"}},
		wantMsg: "queue 1 keys",
	}, {
		name:    "multiple keys",
		keys:    []QueueKey{{Key: "key1"}, {Key: "key2"}, {Key: "key3"}},
		wantMsg: "queue 3 keys",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := QueueKeys(tt.keys...)
			if err.Error() != tt.wantMsg {
				t.Errorf("Error() = %q, want %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestRequeueAfterWithJitter(t *testing.T) {
	for range 100 {
		err := RequeueAfterWithJitter(10*time.Second, 50*time.Second)
		got, floor, ok := GetRequeueOptions(err)
		if !ok || floor {
			t.Fatalf("GetRequeueOptions(%v): got = (%v, %t, %t), want floor = false, ok = true", err, got, floor, ok)
		}
		if got < 10*time.Second || got >= 60*time.Second {
			t.Errorf("delay: got = %v, want in [10s, 60s)", got)
		}
	}

	// Zero jitter must not panic and adds no delay.
	if got, _ := GetRequeueDelay(RequeueAfterWithJitter(10*time.Second, 0)); got != 10*time.Second {
		t.Errorf("delay: got = %v, want = 10s", got)
	}
}

func TestIsInfrastructureError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{{
		name: "nil error",
		err:  nil,
		want: false,
	}, {
		name: "plain error",
		err:  errors.New("boom"),
		want: false,
	}, {
		name: "unavailable status",
		err:  status.Error(codes.Unavailable, "upstream connect error or disconnect/reset before headers"),
		want: true,
	}, {
		name: "wrapped unavailable status",
		err:  fmt.Errorf("calling Process: %w", status.Error(codes.Unavailable, "connection termination")),
		want: true,
	}, {
		name: "internal status",
		err:  status.Error(codes.Internal, "reconcile failed"),
		want: false,
	}, {
		name: "deadline exceeded status",
		err:  status.Error(codes.DeadlineExceeded, "took too long"),
		want: false,
	}, {
		name: "requeue sentinel",
		err:  RequeueAfter(time.Minute),
		want: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsInfrastructureError(tt.err); got != tt.want {
				t.Errorf("IsInfrastructureError: got = %t, want = %t", got, tt.want)
			}
		})
	}
}
