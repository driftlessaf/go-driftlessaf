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
	}, {
		name: "marked requeue sentinel",
		err:  InfrastructureError(RequeueAfter(time.Minute)),
		want: true,
	}, {
		name: "marked plain error",
		err:  InfrastructureError(errors.New("boom")),
		want: true,
	}, {
		name: "marked plain error with causes",
		err:  InfrastructureError(errors.New("boom"), errors.New("dial tcp: connection refused")),
		want: true,
	}, {
		name: "wrapped marked error",
		err:  fmt.Errorf("calling Process: %w", InfrastructureError(errors.New("boom"))),
		want: true,
	}, {
		name: "marker of nil error",
		err:  InfrastructureError(nil),
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

func TestHasInfrastructureMarker(t *testing.T) {
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
		// The status-code class is where the two predicates differ:
		// IsInfrastructureError reports true here, this one does not.
		name: "unavailable status",
		err:  status.Error(codes.Unavailable, "upstream connect error or disconnect/reset before headers"),
		want: false,
	}, {
		name: "wrapped unavailable status",
		err:  fmt.Errorf("calling Process: %w", status.Error(codes.Unavailable, "connection termination")),
		want: false,
	}, {
		name: "requeue sentinel",
		err:  RequeueAfter(time.Minute),
		want: false,
	}, {
		name: "marked requeue sentinel",
		err:  InfrastructureError(RequeueAfter(time.Minute)),
		want: true,
	}, {
		name: "marked plain error",
		err:  InfrastructureError(errors.New("boom")),
		want: true,
	}, {
		name: "wrapped marked error",
		err:  fmt.Errorf("calling Process: %w", InfrastructureError(errors.New("boom"))),
		want: true,
	}, {
		name: "marker of nil error",
		err:  InfrastructureError(nil),
		want: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasInfrastructureMarker(tt.err); got != tt.want {
				t.Errorf("HasInfrastructureMarker: got = %t, want = %t", got, tt.want)
			}
		})
	}
}

func TestInfrastructureErrorIsTransparent(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	err := InfrastructureError(RequeueAfter(5*time.Minute), cause)

	if got, want := err.Error(), "requeue after 5m0s (floor=false)"; got != want {
		t.Errorf("Error(): got = %q, want = %q", got, want)
	}

	delay, floor, ok := GetRequeueOptions(err)
	if !ok || floor {
		t.Fatalf("GetRequeueOptions(%v): got = (%v, %t, %t), want floor = false, ok = true", err, delay, floor, ok)
	}
	if want := 5 * time.Minute; delay != want {
		t.Errorf("delay: got = %v, want = %v", delay, want)
	}

	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(err, cause): got = false, want = true")
	}
	if !IsInfrastructureError(err) {
		t.Errorf("IsInfrastructureError: got = false, want = true")
	}
}

func TestInfrastructureCauses(t *testing.T) {
	first := errors.New("overloaded_error: server is overloaded")
	second := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")

	tests := []struct {
		name string
		err  error
		want []error
	}{{
		name: "nil error",
		err:  nil,
		want: nil,
	}, {
		name: "unmarked error",
		err:  errors.New("boom"),
		want: nil,
	}, {
		name: "marked without causes",
		err:  InfrastructureError(RequeueAfter(5 * time.Minute)),
		want: nil,
	}, {
		name: "marked with one cause",
		err:  InfrastructureError(RequeueAfter(5*time.Minute), first),
		want: []error{first},
	}, {
		name: "marked with several causes",
		err:  InfrastructureError(RequeueAfter(5*time.Minute), first, second),
		want: []error{first, second},
	}, {
		name: "marked with a nil cause",
		err:  InfrastructureError(RequeueAfter(5*time.Minute), nil, first),
		want: []error{first},
	}, {
		name: "wrapped marked error",
		err:  fmt.Errorf("calling Process: %w", InfrastructureError(errors.New("boom"), first)),
		want: []error{first},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InfrastructureCauses(tt.err)
			if len(got) != len(tt.want) {
				t.Fatalf("InfrastructureCauses: got = %v, want = %v", got, tt.want)
			}
			for i, want := range tt.want {
				if !errors.Is(got[i], want) {
					t.Errorf("cause %d: got = %v, want = %v", i, got[i], want)
				}
			}
		})
	}
}

func TestInfrastructureErrorNil(t *testing.T) {
	if err := InfrastructureError(nil, errors.New("cause")); err != nil {
		t.Errorf("InfrastructureError(nil): got = %v, want = nil", err)
	}
}

func TestDeadLetterError(t *testing.T) {
	base := errors.New("permanent refusal")
	err := DeadLetterError(base, "permanent")
	if err == nil {
		t.Fatal("Expected non-nil error")
	}

	d := GetDeadLetterDetails(err)
	if d == nil {
		t.Fatal("GetDeadLetterDetails returned nil for a DeadLetterError")
	}
	if got, want := d.GetMessage(), "permanent"; got != want {
		t.Errorf("GetMessage() = %q, want %q", got, want)
	}

	// A DeadLetterError must ALSO read as non-retriable: a dispatcher
	// predating the marker degrades to the drop path, never a retry loop.
	if nrd := GetNonRetriableDetails(err); nrd == nil {
		t.Error("GetNonRetriableDetails returned nil: an old dispatcher would retry a permanent refusal")
	}

	// The marker must survive the gRPC status round-trip the
	// receiver→dispatcher hop performs.
	s, _ := status.FromError(err)
	if d := GetDeadLetterDetails(s.Err()); d == nil || d.GetMessage() != "permanent" {
		t.Errorf("dead-letter marker did not survive the status round-trip: %v", d)
	}
}

func TestDeadLetterErrorNil(t *testing.T) {
	if err := DeadLetterError(nil, "unused"); err != nil {
		t.Errorf("DeadLetterError(nil) = %v, want nil", err)
	}
}

func TestGetDeadLetterDetailsIgnoresPlainNonRetriable(t *testing.T) {
	err := NonRetriableError(errors.New("plain"), "no retry")
	if d := GetDeadLetterDetails(err); d != nil {
		t.Errorf("GetDeadLetterDetails(NonRetriableError) = %v, want nil (a plain non-retriable must drop, not dead-letter)", d)
	}
	if d := GetDeadLetterDetails(errors.New("bare")); d != nil {
		t.Errorf("GetDeadLetterDetails(bare error) = %v, want nil", d)
	}
	if d := GetDeadLetterDetails(nil); d != nil {
		t.Errorf("GetDeadLetterDetails(nil) = %v, want nil", d)
	}
}
