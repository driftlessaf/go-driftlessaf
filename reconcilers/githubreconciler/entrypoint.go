/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/condcache"
	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/go-grpc-kit/pkg/duplex"
	kmetrics "chainguard.dev/go-grpc-kit/pkg/metrics"
	traceinterceptors "chainguard.dev/go-grpc-kit/pkg/trace"
	"github.com/chainguard-dev/clog"
	"github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
	"github.com/chainguard-dev/terraform-infra-common/pkg/profiler"
	"github.com/google/go-github/v88/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/recovery"
	goenvconfig "github.com/sethvargo/go-envconfig"
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
//
// A MainOption must only record configuration and be free of side effects:
// AppMain applies every option once to read the App options, then again
// inside Main.
type MainOption func(*mainOptions)

// Middleware wraps a ReconcilerFunc, allowing callers to inject logic before
// and/or after each reconcile invocation. Layers are composed so that the
// first argument to WithMiddleware is outermost: WithMiddleware(A, B) produces
// A(B(rec)), matching gRPC interceptor ordering.
type Middleware func(ReconcilerFunc) ReconcilerFunc

type mainOptions struct {
	runner         func(context.Context, workqueue.WorkqueueServiceServer) error
	identity       string
	interceptors   []grpc.UnaryServerInterceptor
	middleware     []Middleware
	tsff           func(identity string) TokenSourceFunc
	reconcilerOpts []Option
	// installIDFunc, when set, is attached to the ClientCache so reconcilers can
	// resolve an org to its GitHub App installation ID (reusing the App's cached
	// lookup) without constructing a second App. Set by AppMain.
	installIDFunc func(ctx context.Context, org string) (int64, error)
	// wrapTransport, when set, wraps each GitHub client's authenticated
	// transport. Set by WithConditionalRequests.
	wrapTransport func(http.RoundTripper) http.RoundTripper
	// clientCacheOpts configure the ClientCache. Set by WithClientCacheOptions.
	clientCacheOpts []ClientCacheOption
	// appOpts configure the App AppMain builds. Set by WithAppOptions.
	appOpts []AppOption
}

// newClientCacheFor builds the ClientCache Main hands to the reconciler
// functor, applying every MainOption that configures the cache rather than
// the server around it.
//
// Extracted from Main so the wiring is reachable from a test. Main itself
// binds a port and serves, so nothing inside it can be exercised directly —
// which meant a dropped assignment here would leave the options that set
// these fields (WithConditionalRequests above) silently inert, with every
// test of the thing they configure still passing.
func newClientCacheFor(mo mainOptions, identity string) *ClientCache {
	cc := NewClientCache(mo.tsff(identity), mo.clientCacheOpts...)
	cc.installIDFunc = mo.installIDFunc
	cc.wrapTransport = mo.wrapTransport
	return cc
}

// WithRunner supplies the serving lifetime for a fully constructed reconciler.
// It preserves the same authentication, cache, attribution, and middleware as
// the gRPC entrypoint. gRPC server interceptors, including those configured by
// WithInterceptors, are not applied. The runner must join admitted work before
// returning; its caller keeps the construction context alive for that entire lifetime.
func WithRunner(run func(context.Context, workqueue.WorkqueueServiceServer) error) MainOption {
	return func(o *mainOptions) { o.runner = run }
}

// WithInterceptors adds gRPC unary server interceptors that run before
// the default metrics and recovery interceptors. It has no effect with WithRunner.
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

// WithConditionalRequests makes every GitHub GET this reconciler issues a
// conditional request: responses are remembered with their ETag, re-reads
// revalidate with If-None-Match, and an unchanged resource is served from
// memory. A 304 does not count against the App installation's hourly REST
// budget, so a reconciler that re-reads the same pull request on every event
// for it pays for the first read and nothing after it.
//
// It buys nothing for a URL read once — a resource addressed by commit SHA is
// a new URL every time — so the saving tracks how often the same resource is
// re-read, not how many calls are made.
//
// The cache is created per client, which the ClientCache already scopes per
// (org, repo). That is deliberate and is the safety property: a remembered
// body is only ever served back through the credentials that fetched it. See
// the condcache package doc.
//
// Off by default. Nothing is served without asking GitHub, so enabling it
// cannot serve stale data; the cost is memory, bounded by the options passed
// here.
func WithConditionalRequests(opts ...condcache.Option) MainOption {
	return func(o *mainOptions) {
		o.wrapTransport = func(base http.RoundTripper) http.RoundTripper {
			return condcache.New(base, opts...)
		}
	}
}

// WithClientCacheOptions configures the ClientCache handed to the reconciler,
// e.g. WithClientCacheSize.
func WithClientCacheOptions(opts ...ClientCacheOption) MainOption {
	return func(o *mainOptions) {
		o.clientCacheOpts = append(o.clientCacheOpts, opts...)
	}
}

// WithAppOptions configures the GitHub App AppMain builds, e.g.
// WithInstallLookupTimeout. Other entrypoints build no App and ignore it.
func WithAppOptions(opts ...AppOption) MainOption {
	return func(o *mainOptions) {
		o.appOpts = append(o.appOpts, opts...)
	}
}

// WithOwnerFilter drops keys whose owner f rejects before any GitHub call is
// made for them; see OwnerFilter. Use it when a reconciler serves only some
// of the owners that can reach its queue, e.g. one subscribed to every
// installation of a shared App.
//
// It applies to the workqueue reconciler Main, AppMain, RepoMain and OrgMain
// serve. CLIMain calls the ReconcilerFunc directly for the keys it is given
// and does not consult it; reconcilers built on the branchreconciler package
// do not use this Reconciler and are unaffected.
func WithOwnerFilter(f OwnerFilter) MainOption {
	return withReconcilerOptions(WithReconcilerOwnerFilter(f))
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
// GitHub App. It reads GITHUB_APP_ID and GITHUB_APP_KEY (a gcpkms:// or
// file:// URI, per NewApp) from the environment, creates the app token
// source, and delegates to Main.
// OCTO_IDENTITY is still required and used as the reconciler identity (e.g.
// for PR author names and bot display names).
func AppMain[T any](ctx context.Context, f Functor[T], opts ...MainOption) error {
	var appEnv struct {
		AppID  int64  `env:"GITHUB_APP_ID,required"`
		AppKey string `env:"GITHUB_APP_KEY,required"`
	}
	if err := goenvconfig.Process(ctx, &appEnv); err != nil {
		return fmt.Errorf("process GitHub App environment config: %w", err)
	}

	app, err := newAppFor(ctx, appEnv.AppID, appEnv.AppKey, opts)
	if err != nil {
		return fmt.Errorf("create GitHub App: %w", err)
	}

	return Main(ctx, f, append(opts,
		WithTokenSourceFuncFactory(func(_ string) TokenSourceFunc {
			return app.TokenSourceFunc()
		}),
		// Expose the App's cached installation-ID lookup on the ClientCache so
		// reconcilers can resolve org -> installation ID without building a
		// second App (the token minting already primes this cache).
		func(o *mainOptions) { o.installIDFunc = app.LookupInstallID },
		// Prepend rather than append: attribution goes outermost so every
		// layer below it — caller middleware included — reconciles on a
		// context whose GitHub calls carry the app_id/installation_id labels.
		// This option runs after the caller's (AppMain appends its own opts),
		// so the caller's layers are already present to be wrapped.
		func(o *mainOptions) {
			o.middleware = append([]Middleware{appAttribution(appEnv.AppID, app.LookupInstallID)}, o.middleware...)
		},
	)...)
}

// newAppFor builds AppMain's App, applying the App options among opts. It
// reads them ahead of Main, which applies every option again.
func newAppFor(ctx context.Context, appID int64, keyURI string, opts []MainOption) (*App, error) {
	var mo mainOptions
	for _, o := range opts {
		o(&mo)
	}
	return NewApp(ctx, appID, keyURI, mo.appOpts...)
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
	for _, layer := range slices.Backward(layers) {
		rec = layer(rec)
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
	if err := goenvconfig.Process(ctx, env); err != nil {
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

	clientCache := newClientCacheFor(mo, identity)

	rec, err := f(ctx, identity, clientCache, env.Config)
	if err != nil {
		return fmt.Errorf("create reconciler: %w", err)
	}
	rec = applyMiddleware(rec, mo.middleware)

	service := NewReconciler(clientCache, append([]Option{WithReconciler(rec)}, mo.reconcilerOpts...)...)
	if mo.runner != nil {
		return mo.runner(ctx, service)
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

	workqueue.RegisterWorkqueueServiceServer(d.Server, service)

	healthgrpc.RegisterHealthServer(d.Server, health.NewServer())

	d.RegisterListenAndServeMetrics(env.MetricsPort, env.EnablePprof)

	// ListenAndServe does not watch ctx, so the signal is bridged here: on
	// SIGTERM (or any other ctx cancellation) the server is shut down with a
	// bounded deadline, shorter than Cloud Run's grace period, so the process
	// exits cleanly instead of being SIGKILLed once that grace period elapses.
	// ListenAndServe runs in its own goroutine so Main can wait on whichever
	// of ctx.Done() or the serve error arrives first; if ctx wins, d.Shutdown
	// drains in-flight requests before Main waits for ListenAndServe to
	// actually return, rather than racing ahead the moment it reports
	// http.ErrServerClosed, which happens as soon as shutdown starts.
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- d.ListenAndServe(ctx)
	}()

	clog.InfoContext(ctx, "Starting reconciler", "port", env.Port)

	var serveErr error
	select {
	case <-ctx.Done():
		// One line as the drain starts and one as it ends, so a SIGKILL in
		// between shows which side of the deadline it landed on. The cause
		// names the signal when signal.NotifyContext cancelled ctx.
		start := time.Now()
		clog.InfoContext(ctx, "reconciler server draining", "cause", context.Cause(ctx), "deadline", shutdownGrace.String())
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		if err := d.Shutdown(shutdownCtx); err != nil {
			clog.WarnContext(ctx, "reconciler server shutdown did not finish cleanly", "error", err, "elapsed", time.Since(start).String())
		} else {
			clog.InfoContext(ctx, "reconciler server drained", "elapsed", time.Since(start).String())
		}
		serveErr = <-serveErrCh
	case serveErr = <-serveErrCh:
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// shutdownGrace bounds how long Main waits for the duplex server to shut
// down once ctx is cancelled, so it stays well inside Cloud Run's SIGTERM to
// SIGKILL grace period.
const shutdownGrace = 8 * time.Second

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
	cc := NewClientCache(tsf, mo.clientCacheOpts...)

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
