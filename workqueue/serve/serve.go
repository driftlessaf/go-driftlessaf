/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package serve

import (
	"context"
	"errors"
	"net/http"
	"time"

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

// shutdownGrace bounds the drain once ctx is cancelled, so it stays inside
// Cloud Run's SIGTERM to SIGKILL grace period.
const shutdownGrace = 8 * time.Second

// ListenAndServe registers srv and a health service on a duplex gRPC server,
// starts the metrics listener and memusage.Heartbeat, and blocks until ctx is
// cancelled or the server fails. On cancellation it drains in-flight requests
// for up to shutdownGrace and returns nil. The metrics listener and the memory
// heartbeat run for the life of the process, not of the call, so a second call
// with the same metrics port in one process exits the process. Callers set up
// profiling, metrics, and tracing exporters before calling it, as
// githubreconciler.Main does.
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

	// duplex.ListenAndServe does not watch ctx, so cancellation is bridged to
	// Shutdown here. Shutdown blocks until in-flight requests finish or
	// shutdownGrace passes; the receive afterwards joins the serve goroutine
	// and keeps a real serve error.
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- d.ListenAndServe(ctx)
	}()

	var serveErr error
	select {
	case <-ctx.Done():
		start := time.Now()
		clog.InfoContext(ctx, "workqueue server draining", "cause", context.Cause(ctx), "deadline", shutdownGrace.String())
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		if err := d.Shutdown(shutdownCtx); err != nil {
			clog.WarnContext(ctx, "workqueue server shutdown did not finish cleanly", "error", err, "elapsed", time.Since(start).String())
		} else {
			clog.InfoContext(ctx, "workqueue server drained", "elapsed", time.Since(start).String())
		}
		serveErr = <-serveErrCh
	case serveErr = <-serveErrCh:
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}
