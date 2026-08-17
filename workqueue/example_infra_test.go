/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue_test

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"chainguard.dev/driftlessaf/workqueue"
)

// ExampleIsInfrastructureError demonstrates how dispatch errors are
// classified: transport-level failures (the receiver died mid-call, no
// healthy backend, a dependency reporting itself unavailable) are
// infrastructure errors. The classification is observability-only — it
// separates infrastructure churn from application failures on dispatch
// error events — while scheduling retries every failure on the same
// widening backoff curve.
func ExampleIsInfrastructureError() {
	// The gRPC transport synthesizes codes.Unavailable when the receiving
	// instance is killed mid-dispatch.
	infra := status.Error(codes.Unavailable, "connection termination")
	fmt.Println(workqueue.IsInfrastructureError(infra))

	// An ordinary application failure is not infrastructure.
	app := errors.New("reconcile failed")
	fmt.Println(workqueue.IsInfrastructureError(app))

	// Output:
	// true
	// false
}

// ExampleInfrastructureError demonstrates marking a requeue error as an
// infrastructure failure when only the call site can recognise the provider
// fault. The marker is transparent: the message, the requeue delay, and the
// original cause all stay reachable.
func ExampleInfrastructureError() {
	cause := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	err := workqueue.InfrastructureError(workqueue.RequeueAfter(5*time.Minute), cause)

	fmt.Println(err)
	fmt.Println(workqueue.IsInfrastructureError(err))

	delay, _ := workqueue.GetRequeueDelay(err)
	fmt.Println(delay)
	fmt.Println(errors.Is(err, cause))

	// Output:
	// requeue after 5m0s (floor=false)
	// true
	// 5m0s
	// true
}

// ExampleHasInfrastructureMarker demonstrates the narrower predicate: it
// matches only an error a call site marked with InfrastructureError, and never
// the codes.Unavailable class that IsInfrastructureError also accepts. A caller
// that must exempt a marked provider failure, but still count a bare
// codes.Unavailable, uses this one.
func ExampleHasInfrastructureMarker() {
	marked := workqueue.InfrastructureError(workqueue.RequeueAfter(5*time.Minute), errors.New("connection reset by peer"))
	fmt.Println(workqueue.HasInfrastructureMarker(marked))

	// An unmarked codes.Unavailable carries no marker.
	unavailable := status.Error(codes.Unavailable, "connection termination")
	fmt.Println(workqueue.HasInfrastructureMarker(unavailable))
	fmt.Println(workqueue.IsInfrastructureError(unavailable))

	// Output:
	// true
	// false
	// true
}

// ExampleInfrastructureCauses demonstrates reading the provider causes back off
// a marked error. The marker's message is the wrapped requeue's message, so a
// caller that reports the failure in text needs the causes to name the provider
// failure behind it.
func ExampleInfrastructureCauses() {
	cause := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	err := workqueue.InfrastructureError(workqueue.RequeueAfter(5*time.Minute), cause)

	for _, c := range workqueue.InfrastructureCauses(err) {
		fmt.Println(c)
	}

	// An unmarked error carries no causes.
	fmt.Println(workqueue.InfrastructureCauses(errors.New("reconcile failed")))

	// Output:
	// dial tcp 10.0.0.1:443: connect: connection refused
	// []
}
