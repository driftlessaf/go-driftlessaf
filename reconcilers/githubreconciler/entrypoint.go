/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/go-grpc-kit/pkg/duplex"
	kmetrics "chainguard.dev/go-grpc-kit/pkg/metrics"
	traceinterceptors "chainguard.dev/go-grpc-kit/pkg/trace"
	"github.com/chainguard-dev/clog"
	"github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
	"github.com/chainguard-dev/terraform-infra-common/pkg/profiler"
	"github.com/google/go-github/v84/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/recovery"
	"github.com/sethvargo/go-envconfig"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
)

// Functor constructs a ReconcilerFunc from the given context, identity,
// token source, and user-provided configuration. The type parameter T is
// the user's config struct which is populated via envconfig.
type Functor[T any] func(
	ctx context.Context,
	identity string,
	tokenSourceFor TokenSourceFunc,
	cfg T,
) (ReconcilerFunc, error)

// MainOption configures the behavior of RepoMain/OrgMain.
type MainOption func(*mainOptions)

type mainOptions struct {
	interceptors []grpc.UnaryServerInterceptor
}

// WithInterceptors adds gRPC unary server interceptors that run before
// the default metrics and recovery interceptors.
func WithInterceptors(inter ...grpc.UnaryServerInterceptor) MainOption {
	return func(o *mainOptions) {
		o.interceptors = append(o.interceptors, inter...)
	}
}

// RepoMain is the entrypoint for reconcilers that use repo-scoped GitHub
// credentials. It parses environment configuration, sets up metrics and
// tracing, creates the gRPC server, and runs the reconciler.
func RepoMain[T any](ctx context.Context, f Functor[T], opts ...MainOption) error {
	return commonMain(ctx, f, func(identity string) TokenSourceFunc {
		return func(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
			return NewRepoTokenSource(ctx, identity, org, repo), nil
		}
	}, nil, opts...)
}

// OrgMain is the entrypoint for reconcilers that use org-scoped GitHub
// credentials. It parses environment configuration, sets up metrics and
// tracing, creates the gRPC server, and runs the reconciler.
func OrgMain[T any](ctx context.Context, f Functor[T], opts ...MainOption) error {
	return commonMain(ctx, f, func(identity string) TokenSourceFunc {
		return func(ctx context.Context, org, _ string) (oauth2.TokenSource, error) {
			return NewOrgTokenSource(ctx, identity, org), nil
		}
	}, []Option{WithOrgScopedCredentials()}, opts...)
}

func commonMain[T any](
	ctx context.Context,
	f Functor[T],
	tsff func(identity string) TokenSourceFunc,
	reconcilerOpts []Option,
	opts ...MainOption,
) error {
	var mo mainOptions
	for _, o := range opts {
		o(&mo)
	}
	env := &struct {
		Config T

		Port         int    `env:"PORT,default=8080"`
		OctoIdentity string `env:"OCTO_IDENTITY,required"`
		MetricsPort  int    `env:"METRICS_PORT,default=2112"`
		EnablePprof  bool   `env:"ENABLE_PPROF,default=false"`
	}{}
	if err := envconfig.Process(ctx, env); err != nil {
		return fmt.Errorf("process environment config: %w", err)
	}

	profiler.SetupProfiler()
	defer httpmetrics.SetupMetrics(ctx)()
	defer httpmetrics.SetupTracer(ctx)()

	tsf := tsff(env.OctoIdentity)

	// Create GitHub client cache with Octo identity
	clientCache := NewClientCache(tsf)

	// Create the reconciler
	rec, err := f(ctx, env.OctoIdentity, tsf, env.Config)
	if err != nil {
		return fmt.Errorf("create reconciler: %w", err)
	}

	// Create duplex server (HTTP + gRPC on same port) with metrics and tracing
	d := duplex.New(
		env.Port,
		grpc.StatsHandler(traceinterceptors.RestoreTraceParentHandler),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainStreamInterceptor(kmetrics.StreamServerInterceptor()),
		grpc.ChainUnaryInterceptor(append(
			mo.interceptors,
			kmetrics.UnaryServerInterceptor(),
			recovery.UnaryServerInterceptor(), // must be last
		)...),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)

	// Register workqueue service with the GitHub reconciler
	workqueue.RegisterWorkqueueServiceServer(d.Server, NewReconciler(
		clientCache,
		append([]Option{WithReconciler(rec)}, reconcilerOpts...)...,
	))

	// Register health check handler
	healthgrpc.RegisterHealthServer(d.Server, health.NewServer())

	// Initialize gRPC prometheus metrics and start metrics/pprof server
	d.RegisterListenAndServeMetrics(env.MetricsPort, env.EnablePprof)

	// Start the server
	clog.InfoContext(ctx, "Starting reconciler", "port", env.Port)
	return d.ListenAndServe(ctx)
}

// ghTokenSource is an oauth2.TokenSource that shells out to 'gh auth token'.
type ghTokenSource struct{}

func (ghTokenSource) Token() (*oauth2.Token, error) {
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return nil, fmt.Errorf("gh auth token: %w", err)
	}
	return &oauth2.Token{
		AccessToken: strings.TrimSpace(string(out)),
	}, nil
}

// CLIMain runs a reconciler locally in a loop, useful for development and
// testing. It uses 'gh auth token' for GitHub authentication. Each key is
// reconciled in its own goroutine with a 1m delay between iterations.
// The function blocks until ctx is cancelled.
func CLIMain[T any](ctx context.Context, f Functor[T], identity string, cfg T, keys []string) error {
	// Parse all keys upfront to fail fast on bad URLs.
	resources := make([]*Resource, 0, len(keys))
	for _, key := range keys {
		res, err := ParseURL(key)
		if err != nil {
			return fmt.Errorf("parse key %q: %w", key, err)
		}
		resources = append(resources, res)
	}

	tsf := func(_ context.Context, _, _ string) (oauth2.TokenSource, error) {
		return ghTokenSource{}, nil
	}

	rec, err := f(ctx, identity, tsf, cfg)
	if err != nil {
		return fmt.Errorf("create reconciler: %w", err)
	}

	gh := github.NewClient(oauth2.NewClient(ctx, ghTokenSource{}))

	clog.InfoContext(ctx, "Starting reconciler loop", "identity", identity, "keys", len(keys))

	var wg sync.WaitGroup
	for _, res := range resources {
		wg.Go(func() {
			for {
				clog.InfoContext(ctx, "Reconciling", "url", res.URL)
				if err := rec(ctx, res, gh); err != nil {
					clog.ErrorContext(ctx, "Reconcile failed", "url", res.URL, "error", err)
				}

				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Minute):
				}
			}
		})
	}

	wg.Wait()
	return ctx.Err()
}
