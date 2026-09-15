/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package serve_test

import (
	"context"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/driftlessaf/workqueue/serve"
	"google.golang.org/grpc"
)

// Example shows the shape of a main that assembles its own workqueue server.
// Environment parsing, credentials, and reconciler construction stay in the
// caller; ListenAndServe only owns the gRPC plumbing.
func Example() {
	var (
		ctx          context.Context
		srv          workqueue.WorkqueueServiceServer // e.g. from linearreconciler.New
		port         int                              // from envconfig
		interceptors []grpc.UnaryServerInterceptor
	)
	_ = serve.ListenAndServe(ctx, srv,
		serve.WithPort(port),
		serve.WithInterceptors(interceptors...),
	)
}
