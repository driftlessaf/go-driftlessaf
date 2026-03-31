/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"errors"
	"fmt"
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
// client cache, and user-provided configuration. The type parameter T is
// the user's config struct which is populated via envconfig.
type Functor[T any] func(
	ctx context.Context,
	identity string,
	cc *ClientCache,
	cfg T,
) (ReconcilerFunc, error)

// MainOption configures the behavior of Main and its wrappers.
type MainOption func(*mainOptions)

type mainOptions struct {
	interceptors   []grpc.UnaryServerInterceptor
	tsff           func(identity string) TokenSourceFunc
	reconcilerOpts []Option
}

// WithInterceptors adds gRPC unary server interceptors that run before
// the default metrics and recovery interceptors.
func WithInterceptors(inter ...grpc.UnaryServerInterceptor) MainOption {
	return func(o *mainOptions) {
		o.interceptors = append(o.interceptors, inter...)
	}
}

// WithTokenSourceFuncFactory sets the factory that maps an identity string
// to a TokenSourceFunc. The identity is read from the OCTO_IDENTITY
// environment variable at startup and forwarded to f, which returns the
// TokenSourceFunc used to authenticate GitHub API calls.
//
// Use this to supply custom authentication; for the standard Octo STS cases
// prefer RepoMain or OrgMain.
func WithTokenSourceFuncFactory(f func(identity string) TokenSourceFunc) MainOption {
	return func(o *mainOptions) {
		o.tsff = f
	}
}

// withReconcilerOptions appends options forwarded to the workqueue reconciler.
// Used by OrgMain to enable org-scoped credential handling.
func withReconcilerOptions(opts ...Option) MainOption {
	return func(o *mainOptions) {
		o.reconcilerOpts = append(o.reconcilerOpts, opts...)
	}
}

// AppMain is the entrypoint for reconcilers that authenticate using a dedicated
// GitHub App. It reads GITHUB_APP_ID and GITHUB_APP_KEY (a gcpkms:// URI) from
// the environment, creates the app token source, and delegates to Main.
// OCTO_IDENTITY is still required and used as the reconciler identity (e.g.
// for PR author names and bot display names).
func AppMain[T any](ctx context.Context, f Functor[T], opts ...MainOption) error {
	var appEnv struct {
		AppID  int64  `env:"GITHUB_APP_ID,required"`
		AppKey string `env:"GITHUB_APP_KEY,required"`
	}
	if err := envconfig.Process(ctx, &appEnv); err != nil {
		return fmt.Errorf("process GitHub App environment config: %w", err)
	}

	tsf, err := NewAppTokenSource(ctx, appEnv.AppID, appEnv.AppKey)
	if err != nil {
		return fmt.Errorf("create GitHub App token source: %w", err)
	}

	return Main(ctx, f, append(opts, WithTokenSourceFuncFactory(func(_ string) TokenSourceFunc {
		return tsf
	}))...)
}

// RepoMain is the entrypoint for reconcilers that use repo-scoped GitHub
// credentials via Octo STS. It is a convenience wrapper around Main.
func RepoMain[T any](ctx context.Context, f Functor[T], opts ...MainOption) error {
	return Main(ctx, f, append(opts, WithTokenSourceFuncFactory(func(identity string) TokenSourceFunc {
		return func(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
			return NewRepoTokenSource(ctx, identity, org, repo), nil
		}
	}))...)
}

// OrgMain is the entrypoint for reconcilers that use org-scoped GitHub
// credentials via Octo STS. It is a convenience wrapper around Main.
func OrgMain[T any](ctx context.Context, f Functor[T], opts ...MainOption) error {
	return Main(ctx, f, append(opts,
		WithTokenSourceFuncFactory(func(identity string) TokenSourceFunc {
			return func(ctx context.Context, org, _ string) (oauth2.TokenSource, error) {
				return NewOrgTokenSource(ctx, identity, org), nil
			}
		}),
		withReconcilerOptions(WithOrgScopedCredentials()),
	)...)
}

// Main is the core entrypoint for GitHub reconcilers. It parses environment
// configuration, sets up metrics and tracing, creates the gRPC server, and
// runs the reconciler.
//
// The token source and reconciler options are configured via MainOption
// functional options. Use RepoMain or OrgMain for the common Octo STS cases,
// or pass WithTokenSourceFuncFactory and related options directly for custom
// authentication (e.g. a dedicated GitHub App).
//
// OCTO_IDENTITY is required and is read from the environment and passed to the
// token source factory.
func Main[T any](ctx context.Context, f Functor[T], opts ...MainOption) error {
	var mo mainOptions
	for _, o := range opts {
		o(&mo)
	}

	if mo.tsff == nil {
		return errors.New("no token source factory configured: use RepoMain, OrgMain, or WithTokenSourceFuncFactory")
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

	tsf := mo.tsff(env.OctoIdentity)

	clientCache := NewClientCache(tsf)

	rec, err := f(ctx, env.OctoIdentity, clientCache, env.Config)
	if err != nil {
		return fmt.Errorf("create reconciler: %w", err)
	}

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

	workqueue.RegisterWorkqueueServiceServer(d.Server, NewReconciler(
		clientCache,
		append([]Option{WithReconciler(rec)}, mo.reconcilerOpts...)...,
	))

	healthgrpc.RegisterHealthServer(d.Server, health.NewServer())

	d.RegisterListenAndServeMetrics(env.MetricsPort, env.EnablePprof)

	clog.InfoContext(ctx, "Starting reconciler", "port", env.Port)
	return d.ListenAndServe(ctx)
}

// CLIMain runs a reconciler locally in a loop. Each key is reconciled in its
// own goroutine with a 1m delay between iterations. The function blocks until
// ctx is cancelled.
//
// The tsf parameter provides GitHub credentials for API calls. Use
// NewRepoTokenSource or NewOrgTokenSource to build token sources from
// Octo STS identities.
func CLIMain[T any](ctx context.Context, f Functor[T], identity string, tsf TokenSourceFunc, cfg T, keys []string) error {
	// Parse all keys upfront to fail fast on bad URLs.
	resources := make([]*Resource, 0, len(keys))
	for _, key := range keys {
		res, err := ParseURL(key)
		if err != nil {
			return fmt.Errorf("parse key %q: %w", key, err)
		}
		resources = append(resources, res)
	}

	cc := NewClientCache(tsf)

	rec, err := f(ctx, identity, cc, cfg)
	if err != nil {
		return fmt.Errorf("create reconciler: %w", err)
	}

	// Use the first resource's owner/repo for the top-level github client.
	ts, err := tsf(ctx, resources[0].Owner, resources[0].Repo)
	if err != nil {
		return fmt.Errorf("create token source: %w", err)
	}
	gh := github.NewClient(oauth2.NewClient(ctx, ts))

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
