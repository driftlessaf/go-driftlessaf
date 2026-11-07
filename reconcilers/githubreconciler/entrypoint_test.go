/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestMainShutsDownOnContextCancellation verifies that Main's duplex server
// is shut down promptly when ctx is cancelled (e.g. on SIGTERM), rather than
// blocking forever, which would leave the process to be SIGKILLed once
// Cloud Run's grace period elapses.
func TestMainShutsDownOnContextCancellation(t *testing.T) {
	t.Setenv("PORT", "0")
	t.Setenv("METRICS_PORT", "0")

	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- Main(ctx, func(_ context.Context, _ string, _ *ClientCache, _ struct{}) (ReconcilerFunc, error) {
			return func(_ context.Context, _ *Resource, _ *github.Client) error {
				return nil
			}, nil
		}, WithIdentity("fixture"),
			WithTokenSourceFuncFactory(func(string) TokenSourceFunc {
				return func(_ context.Context, _, _ string) (oauth2.TokenSource, error) {
					return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fixture"}), nil
				}
			}))
	}()

	// Cancelling immediately races the shutdown goroutine against
	// ListenAndServe's own startup, but http.Server.Shutdown documents that a
	// Shutdown racing (or preceding) Serve still makes Serve return
	// ErrServerClosed immediately, so the race is harmless and no sleep is
	// needed to "wait for the server to start".
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Main() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Main did not return after ctx was cancelled")
	}
}

// TestMainWaitsForInFlightRequestToDrainOnShutdown verifies that Main does not
// return the moment ListenAndServe reports http.ErrServerClosed: it waits for
// d.Shutdown to finish draining, so a reconcile in progress when ctx is
// cancelled gets to complete rather than being cut off mid-request.
func TestMainWaitsForInFlightRequestToDrainOnShutdown(t *testing.T) {
	// A real listener is needed (not the WithRunner seam, which bypasses the
	// duplex server entirely): pick a free port, release it, and hand it to
	// Main via PORT so a real gRPC call can be made against it.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	if err := lis.Close(); err != nil {
		t.Fatalf("lis.Close() = %v", err)
	}

	t.Setenv("PORT", strconv.Itoa(port))
	t.Setenv("METRICS_PORT", "0")

	ctx, cancel := context.WithCancel(t.Context())

	started := make(chan struct{})
	release := make(chan struct{})
	// order records, in the sequence they happen, which of the handler
	// finishing and Main returning comes first. A buggy Main that returns as
	// soon as ListenAndServe reports http.ErrServerClosed (instead of waiting
	// for the drain) would record "main" before "handler"; this is a causal
	// check on channel order, not a race against a timer.
	order := make(chan string, 2)

	mainErrCh := make(chan error, 1)
	go func() {
		err := Main(ctx, func(_ context.Context, _ string, _ *ClientCache, _ struct{}) (ReconcilerFunc, error) {
			return func(_ context.Context, _ *Resource, _ *github.Client) error {
				close(started)
				<-release
				order <- "handler"
				return nil
			}, nil
		}, WithIdentity("fixture"),
			WithTokenSourceFuncFactory(func(string) TokenSourceFunc {
				return func(_ context.Context, _, _ string) (oauth2.TokenSource, error) {
					return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fixture"}), nil
				}
			}))
		order <- "main"
		mainErrCh <- err
	}()

	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient() = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := workqueue.NewWorkqueueServiceClient(conn)

	callErrCh := make(chan error, 1)
	go func() {
		callCtx, callCancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer callCancel()
		// WaitForReady blocks the call until the server is actually
		// listening, rather than failing fast while it is still starting up.
		_, err := client.Process(callCtx, &workqueue.ProcessRequest{Key: "https://github.com/octo/repo/issues/1"}, grpc.WaitForReady(true))
		callErrCh <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile handler did not start")
	}

	// Cancel while the handler is in flight, then let it finish. Main should
	// wait for the handler (and thus the drain) to complete before returning.
	cancel()
	close(release)

	first := <-order
	second := <-order
	if first != "handler" || second != "main" {
		t.Fatalf("event order = [%s, %s], want [handler, main]", first, second)
	}

	if err := <-mainErrCh; err != nil {
		t.Fatalf("Main() = %v, want nil", err)
	}
	if err := <-callErrCh; err != nil {
		t.Fatalf("Process() = %v, want nil", err)
	}
}

// TestMainWaitsForHandlerOfCancelledCall verifies that Main waits for a
// reconcile whose caller already cancelled the call. On SIGTERM the workqueue
// dispatcher cancels its in-flight calls before this server drains. gRPC's
// ServeHTTP returns as soon as the client is gone, but the handler keeps
// running its cleanup, such as closing a microVM session. If Main returned
// then, the process would exit with that cleanup unfinished.
//
// The handler's cleanup outlasts Main itself, so a Main that waits for it can
// only return once shutdownGrace runs out, and says so in its log; a Main that
// does not wait reports a clean drain. Whether Main waited is read from that
// outcome rather than from how long the test watched Main not return, at the
// cost of the test taking shutdownGrace.
func TestMainWaitsForHandlerOfCancelledCall(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	if err := lis.Close(); err != nil {
		t.Fatalf("lis.Close() = %v", err)
	}

	t.Setenv("PORT", strconv.Itoa(port))
	t.Setenv("METRICS_PORT", "0")

	logs := &recordingHandler{}
	ctx, cancel := context.WithCancel(clog.WithLogger(t.Context(), clog.New(logs)))

	started := make(chan struct{})
	cancelled := make(chan struct{})
	mainDone := make(chan struct{})

	mainErrCh := make(chan error, 1)
	go func() {
		defer close(mainDone)
		mainErrCh <- Main(ctx, func(_ context.Context, _ string, _ *ClientCache, _ struct{}) (ReconcilerFunc, error) {
			return func(ctx context.Context, _ *Resource, _ *github.Client) error {
				close(started)
				<-ctx.Done()
				close(cancelled)
				// The cleanup a cancelled reconcile still runs, here one that
				// lasts until Main returns.
				<-mainDone
				return ctx.Err()
			}, nil
		}, WithIdentity("fixture"),
			WithTokenSourceFuncFactory(func(string) TokenSourceFunc {
				return func(_ context.Context, _, _ string) (oauth2.TokenSource, error) {
					return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fixture"}), nil
				}
			}))
	}()

	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient() = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := workqueue.NewWorkqueueServiceClient(conn)

	callCtx, callCancel := context.WithCancel(t.Context())
	defer callCancel()
	go func() {
		_, _ = client.Process(callCtx, &workqueue.ProcessRequest{Key: "https://github.com/octo/repo/issues/1"}, grpc.WaitForReady(true))
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile handler did not start")
	}

	// The dispatcher cancels its call, then the server is told to stop.
	callCancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile handler did not see its call cancelled")
	}
	cancel()

	if err := <-mainErrCh; err != nil {
		t.Fatalf("Main(): got = %v, wanted = nil", err)
	}
	if got := logs.shutdownError(); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("shutdown outcome: got = %v, wanted = %v from waiting on the handler", got, context.DeadlineExceeded)
	}
}

// recordingHandler is a slog.Handler that keeps every record logged through
// it.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (*recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

// shutdownError returns the error Main logged when its server shutdown did
// not finish cleanly, or nil if Main logged none.
func (h *recordingHandler) shutdownError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message != "reconciler server shutdown did not finish cleanly" {
			continue
		}
		var err error
		r.Attrs(func(a slog.Attr) bool {
			if a.Key != "error" {
				return true
			}
			err, _ = a.Value.Any().(error)
			return false
		})
		return err
	}
	return nil
}
