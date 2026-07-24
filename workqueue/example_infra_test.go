/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue_test

import (
	"errors"
	"fmt"

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
