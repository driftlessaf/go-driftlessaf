/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
)

var processInfo = &grpc.UnaryServerInfo{
	FullMethod: "/" + WorkqueueService_ServiceDesc.ServiceName + "/Process",
}

// requireRequeue asserts resp is a drain requeue: a ProcessResponse whose
// RequeueAfterSeconds falls within [DrainRequeueDelay, delay+jitter).
func requireRequeue(t *testing.T, resp any, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("err: got = %v, want = nil", err)
	}
	pr, ok := resp.(*ProcessResponse)
	if !ok {
		t.Fatalf("response type: got = %T, want = *ProcessResponse", resp)
	}
	minS := int64(DrainRequeueDelay.Seconds())
	maxS := int64((DrainRequeueDelay + DrainRequeueJitter).Seconds())
	if got := pr.GetRequeueAfterSeconds(); got < minS || got >= maxS {
		t.Errorf("RequeueAfterSeconds: got = %d, want in [%d, %d)", got, minS, maxS)
	}
}

func TestDrainInterceptorPassthroughWhenNotDraining(t *testing.T) {
	drain, cancel := context.WithCancel(t.Context())
	defer cancel()
	icpt := DrainInterceptor(drain)

	want := &ProcessResponse{RequeueAfterSeconds: 7}
	resp, err := icpt(t.Context(), &ProcessRequest{Key: "k"}, processInfo,
		func(ctx context.Context, req any) (any, error) { return want, nil })
	if err != nil || resp != want {
		t.Errorf("passthrough: got = (%v, %v), want = (%v, nil)", resp, err, want)
	}
}

func TestDrainInterceptorIgnoresOtherMethods(t *testing.T) {
	drain, cancel := context.WithCancel(t.Context())
	cancel() // already draining
	icpt := DrainInterceptor(drain)

	called := false
	_, err := icpt(t.Context(), struct{}{},
		&grpc.UnaryServerInfo{FullMethod: "/" + WorkqueueService_ServiceDesc.ServiceName + "/GetKeyState"},
		func(ctx context.Context, req any) (any, error) { called = true; return nil, nil })
	if err != nil {
		t.Fatalf("err: got = %v, want = nil", err)
	}
	if !called {
		t.Error("handler called: got = false, want = true (non-Process methods pass through)")
	}
}

func TestDrainInterceptorRefusesAtArrivalWhileDraining(t *testing.T) {
	drain, cancel := context.WithCancel(t.Context())
	cancel()
	icpt := DrainInterceptor(drain)

	resp, err := icpt(t.Context(), &ProcessRequest{Key: "k"}, processInfo,
		func(ctx context.Context, req any) (any, error) {
			t.Error("handler invoked: got = true, want = false (work must not start while draining)")
			return nil, nil
		})
	requireRequeue(t, resp, err)
}

func TestDrainInterceptorTranslatesCooperativeUnwind(t *testing.T) {
	drain, cancel := context.WithCancel(t.Context())
	icpt := DrainInterceptor(drain)

	started := make(chan struct{})
	go func() {
		<-started
		cancel() // begin draining once the handler is in flight
	}()
	resp, err := icpt(t.Context(), &ProcessRequest{Key: "k"}, processInfo,
		func(ctx context.Context, req any) (any, error) {
			close(started)
			<-ctx.Done() // cooperative: unwind when the request context cancels
			return nil, ctx.Err()
		})
	requireRequeue(t, resp, err)
}

func TestDrainInterceptorKeepsSuccessDuringDrain(t *testing.T) {
	drain, cancel := context.WithCancel(t.Context())
	icpt := DrainInterceptor(drain)

	want := &ProcessResponse{}
	started := make(chan struct{})
	go func() {
		<-started
		cancel()
	}()
	resp, err := icpt(t.Context(), &ProcessRequest{Key: "k"}, processInfo,
		func(ctx context.Context, req any) (any, error) {
			close(started)
			<-ctx.Done() // observe the drain, but the work already finished
			return want, nil
		})
	if err != nil || resp != want {
		t.Errorf("success during drain: got = (%v, %v), want = (%v, nil)", resp, err, want)
	}
}

func TestDrainInterceptorBackstopsUncooperativeHandler(t *testing.T) {
	prev := DrainBackstop
	DrainBackstop = 50 * time.Millisecond
	t.Cleanup(func() { DrainBackstop = prev })

	drain, cancel := context.WithCancel(t.Context())
	icpt := DrainInterceptor(drain)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	go func() {
		<-started
		cancel()
	}()
	resp, err := icpt(t.Context(), &ProcessRequest{Key: "k"}, processInfo,
		func(ctx context.Context, req any) (any, error) {
			close(started)
			<-release // ignores ctx: only the backstop can answer
			return nil, errors.New("too late")
		})
	requireRequeue(t, resp, err)
}

func TestDrainInterceptorPreservesOwnFailures(t *testing.T) {
	drain, cancel := context.WithCancel(t.Context())
	defer cancel()
	icpt := DrainInterceptor(drain)

	want := errors.New("reconcile failed")
	_, err := icpt(t.Context(), &ProcessRequest{Key: "k"}, processInfo,
		func(ctx context.Context, req any) (any, error) { return nil, want })
	if !errors.Is(err, want) {
		t.Errorf("err: got = %v, want = %v (real failures must pass through)", err, want)
	}
}
