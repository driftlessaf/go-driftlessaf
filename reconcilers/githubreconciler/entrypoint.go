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
	"github.com/google/go-github/v88/github"
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

// Middleware wraps a ReconcilerFunc, allowing callers to inject logic before
// and/or after each reconcile invocation. Layers are composed so that the
// first argument to WithMiddleware is outermost: WithMiddleware(A, B) produces
// A(B(rec)), matching gRPC interceptor ordering.
type Middleware func(ReconcilerFunc) ReconcilerFunc

type mainOptions struct {
	identity       string
	interceptors   []grpc.UnaryServerInterceptor
	middleware     []Middleware
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

// WithIdentity sets the reconciler identity, overriding the OCTO_IDENTITY
// environment variable. Exactly one of WithIdentity or OCTO_IDENTITY must be
// provided.
func WithIdentity(identity string) MainOption {
	return func(o *mainOptions) {
		o.identity = identity
	}
}

// WithMiddleware appends middleware layers applied to the ReconcilerFunc before
// it is invoked. The first argument is outermost: WithMiddleware(A, B) produces
// A(B(rec)), matching gRPC interceptor ordering.
func WithMiddleware(m ...Middleware) MainOption {
	return func(o *mainOptions) {
		o.middleware = append(o.middleware, m...)
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

	app, err := NewApp(ctx, appEnv.AppID, appEnv.AppKey)
	if err != nil {
		return fmt.Errorf("create GitHub App: %w", err)
	}

	return Main(ctx, f, append(opts, WithTokenSourceFuncFactory(func(_ string) TokenSourceFunc {
		return app.TokenSourceFunc()
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

// applyMiddleware wraps rec with each layer in order, so that the first layer
// in the slice is outermost. An empty slice returns rec unchanged.
func applyMiddleware(rec ReconcilerFunc, layers []Middleware) ReconcilerFunc {
	for i := len(layers) - 1; i >= 0; i-- {
		rec = layers[i](rec)
	}
	return rec
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
		OctoIdentity string `env:"OCTO_IDENTITY"`
		MetricsPort  int    `env:"METRICS_PORT,default=2112"`
		EnablePprof  bool   `env:"ENABLE_PPROF,default=false"`
	}{}
	if err := envconfig.Process(ctx, env); err != nil {
		return fmt.Errorf("process environment config: %w", err)
	}

	identity := mo.identity
	if identity == "" {
		identity = env.OctoIdentity
	}
	if identity == "" {
		return errors.New("no identity configured: set OCTO_IDENTITY or use WithIdentity")
	}

	profiler.SetupProfiler()
	defer httpmetrics.SetupMetrics(ctx)()
	defer httpmetrics.SetupTracer(ctx)()

	clientCache := NewClientCache(mo.tsff(identity))

	rec, err := f(ctx, identity, clientCache, env.Config)
	if err != nil {
		return fmt.Errorf("create reconciler: %w", err)
	}
	rec = applyMiddleware(rec, mo.middleware)

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
// Use WithIdentity and WithTokenSourceFuncFactory to supply the reconciler
// identity and GitHub credentials respectively.
func CLIMain[T any](ctx context.Context, f Functor[T], cfg T, keys []string, opts ...MainOption) error {
	var mo mainOptions
	for _, o := range opts {
		o(&mo)
	}
	if mo.tsff == nil {
		return errors.New("no token source factory configured: use WithTokenSourceFuncFactory")
	}
	if mo.identity == "" {
		return errors.New("no identity configured: use WithIdentity")
	}

	// Parse all keys upfront to fail fast on bad URLs.
	resources := make([]*Resource, 0, len(keys))
	for _, key := range keys {
		res, err := ParseURL(key)
		if err != nil {
			return fmt.Errorf("parse key %q: %w", key, err)
		}
		resources = append(resources, res)
	}

	tsf := mo.tsff(mo.identity)
	cc := NewClientCache(tsf)

	rec, err := f(ctx, mo.identity, cc, cfg)
	if err != nil {
		return fmt.Errorf("create reconciler: %w", err)
	}
	rec = applyMiddleware(rec, mo.middleware)

	// Use the first resource's owner/repo for the top-level github client.
	ts, err := tsf(ctx, resources[0].Owner, resources[0].Repo)
	if err != nil {
		return fmt.Errorf("create token source: %w", err)
	}
	gh, err := github.NewClient(github.WithHTTPClient(oauth2.NewClient(ctx, ts)))
	if err != nil {
		return fmt.Errorf("create github client: %w", err)
	}

	clog.InfoContext(ctx, "Starting reconciler loop", "identity", mo.identity, "keys", len(keys))

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
