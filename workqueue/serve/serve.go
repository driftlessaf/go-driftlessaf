/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package serve

import (
	"context"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/go-grpc-kit/pkg/duplex"
	kmetrics "chainguard.dev/go-grpc-kit/pkg/metrics"
	traceinterceptors "chainguard.dev/go-grpc-kit/pkg/trace"
	"github.com/chainguard-dev/clog"
	"github.com/chainguard-dev/terraform-infra-common/pkg/memusage"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/recovery"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
)

// Option configures ListenAndServe.
type Option func(*options)

type options struct {
	port         int
	metricsPort  int
	enablePprof  bool
	interceptors []grpc.UnaryServerInterceptor
}

// WithPort sets the gRPC listen port. Defaults to 8080, matching the PORT
// convention Cloud Run uses.
func WithPort(port int) Option {
	return func(o *options) { o.port = port }
}

// WithMetricsPort sets the Prometheus metrics listen port. Defaults to 2112.
func WithMetricsPort(port int) Option {
	return func(o *options) { o.metricsPort = port }
}

// WithPprof exposes pprof handlers on the metrics listener.
func WithPprof(enabled bool) Option {
	return func(o *options) { o.enablePprof = enabled }
}

// WithInterceptors adds gRPC unary server interceptors that run before the
// default metrics and recovery interceptors.
func WithInterceptors(inter ...grpc.UnaryServerInterceptor) Option {
	return func(o *options) { o.interceptors = append(o.interceptors, inter...) }
}

// ListenAndServe registers srv and a health service on a duplex gRPC server,
// starts the metrics listener and memusage.Heartbeat, and blocks until ctx is
// cancelled or the server fails. Callers set up profiling, metrics, and
// tracing exporters before calling it, as githubreconciler.Main does.
func ListenAndServe(ctx context.Context, srv workqueue.WorkqueueServiceServer, opts ...Option) error {
	o := options{port: 8080, metricsPort: 2112}
	for _, opt := range opts {
		opt(&o)
	}

	d := duplex.New(
		o.port,
		grpc.StatsHandler(traceinterceptors.RestoreTraceParentHandler),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainStreamInterceptor(kmetrics.StreamServerInterceptor()),
		grpc.ChainUnaryInterceptor(append(
			o.interceptors,
			kmetrics.UnaryServerInterceptor(),
			recovery.UnaryServerInterceptor(), // must be last
		)...),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)

	workqueue.RegisterWorkqueueServiceServer(d.Server, srv)
	healthgrpc.RegisterHealthServer(d.Server, health.NewServer())
	d.RegisterListenAndServeMetrics(o.metricsPort, o.enablePprof)
	// The heartbeat outlives ctx so it still logs while the server drains.
	go memusage.Heartbeat(context.WithoutCancel(ctx))

	clog.InfoContext(ctx, "Starting workqueue server", "port", o.port)
	return d.ListenAndServe(ctx)
}
