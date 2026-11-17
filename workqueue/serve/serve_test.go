/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package serve_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chainguard-dev/clog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/driftlessaf/workqueue/serve"
)

type unimplementedServer struct {
	workqueue.UnimplementedWorkqueueServiceServer
}

// blockingServer holds each Process call open until release is closed.
type blockingServer struct {
	workqueue.UnimplementedWorkqueueServiceServer
	entered  chan struct{}
	release  chan struct{}
	finished atomic.Bool
}

func (s *blockingServer) Process(context.Context, *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
	close(s.entered)
	<-s.release
	s.finished.Store(true)
	return &workqueue.ProcessResponse{}, nil
}

// The port can be taken between the probe and the bind; that window is
// accepted because serve takes a port, not a listener.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// start runs ListenAndServe in the background and returns once the server
// accepts connections, along with its address and the channel carrying its
// result. A non-nil onReturn runs as soon as ListenAndServe returns, before the
// result is sent.
func start(ctx context.Context, t *testing.T, srv workqueue.WorkqueueServiceServer, onReturn func()) (string, <-chan error) {
	t.Helper()
	port := freePort(t)

	done := make(chan error, 1)
	go func() {
		err := serve.ListenAndServe(ctx, srv, serve.WithPort(port), serve.WithMetricsPort(0))
		if onReturn != nil {
			onReturn()
		}
		done <- err
	}()

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(10 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return addr, done
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not accept connections on %s: %v", addr, err)
		}
		select {
		case err := <-done:
			t.Fatalf("ListenAndServe returned before accepting connections: %v", err)
		case <-ticker.C:
		}
	}
}

func waitReturn(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return within 10s")
		return nil
	}
}

func TestListenAndServeReturnsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	_, done := start(ctx, t, &unimplementedServer{}, nil)

	cancel()
	if err := waitReturn(t, done); err != nil {
		t.Errorf("ListenAndServe: got = %v, want = nil", err)
	}
}

func TestListenAndServeReturnsBindError(t *testing.T) {
	// Hold the port on all interfaces, which is where duplex binds.
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	done := make(chan error, 1)
	go func() {
		done <- serve.ListenAndServe(t.Context(), &unimplementedServer{},
			serve.WithPort(port), serve.WithMetricsPort(0))
	}()

	if err := waitReturn(t, done); err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Errorf("ListenAndServe: got = %v, want = a bind error", err)
	}
}

func TestListenAndServeDrainsInFlightRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	srv := &blockingServer{entered: make(chan struct{}), release: make(chan struct{})}
	// Sampled the moment ListenAndServe returns, so a Shutdown that stopped
	// waiting for the handler is caught however the goroutines interleave.
	var finishedAtReturn bool
	addr, done := start(ctx, t, srv, func() { finishedAtReturn = srv.finished.Load() })

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	rpcErr := make(chan error, 1)
	go func() {
		_, err := workqueue.NewWorkqueueServiceClient(conn).Process(t.Context(), &workqueue.ProcessRequest{})
		rpcErr <- err
	}()
	select {
	case <-srv.entered:
	case err := <-rpcErr:
		t.Fatalf("Process returned before reaching the handler: %v", err)
	}

	cancel()

	// A refused dial means Shutdown has closed the listener, so the drain has
	// begun with the request still in flight.
	deadline := time.Now().Add(10 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still accepting 10s after ctx cancellation")
		}
		<-ticker.C
	}

	close(srv.release)
	if err := <-rpcErr; err != nil {
		t.Errorf("Process: got = %v, want = nil", err)
	}
	if err := waitReturn(t, done); err != nil {
		t.Errorf("ListenAndServe: got = %v, want = nil", err)
	}
	if !finishedAtReturn {
		t.Error("ListenAndServe returned before the in-flight handler finished")
	}
}

// cleanupServer is a Process handler that, once its call is cancelled, runs a
// cleanup lasting until release is closed.
type cleanupServer struct {
	workqueue.UnimplementedWorkqueueServiceServer
	entered   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (s *cleanupServer) Process(ctx context.Context, _ *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
	close(s.entered)
	<-ctx.Done()
	close(s.cancelled)
	<-s.release
	return nil, ctx.Err()
}

// TestListenAndServeWaitsForHandlerOfCancelledCall verifies that
// ListenAndServe waits for a handler whose caller already cancelled the call.
// On SIGTERM the workqueue dispatcher cancels its in-flight calls before this
// server drains. gRPC's ServeHTTP returns as soon as the client is gone, but
// the handler keeps running its cleanup, such as closing a microVM session. If
// ListenAndServe returned then, the process would exit with that cleanup
// unfinished.
//
// The handler's cleanup lasts until ListenAndServe returns, so a
// ListenAndServe that waits for it can only return once shutdownGrace runs
// out, and says so in its log; one that does not wait reports a clean drain,
// and one that waits without a deadline never returns. Whether it waited is
// read from that outcome rather than from how long the test watched it not
// return, at the cost of the test taking shutdownGrace.
func TestListenAndServeWaitsForHandlerOfCancelledCall(t *testing.T) {
	logs := &recordingHandler{}
	ctx, cancel := context.WithCancel(clog.WithLogger(t.Context(), clog.New(logs)))
	defer cancel()

	srv := &cleanupServer{
		entered:   make(chan struct{}),
		cancelled: make(chan struct{}),
		release:   make(chan struct{}),
	}
	addr, done := start(ctx, t, srv, func() { close(srv.release) })

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	callCtx, callCancel := context.WithCancel(t.Context())
	defer callCancel()
	go func() {
		_, _ = workqueue.NewWorkqueueServiceClient(conn).Process(callCtx, &workqueue.ProcessRequest{})
	}()

	select {
	case <-srv.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("Process handler did not start")
	}

	// The dispatcher cancels its call, then the server is told to stop.
	callCancel()
	select {
	case <-srv.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("Process handler did not see its call cancelled")
	}
	cancel()

	// Generous next to shutdownGrace, so a slow runner does not fail it.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe: got = %v, want = nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ListenAndServe did not return within 30s; the handler held shutdown past its deadline")
	}
	if got := logs.shutdownError(); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("shutdown outcome: got = %v, want = %v from waiting on the handler", got, context.DeadlineExceeded)
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

// shutdownError returns the error ListenAndServe logged when its server
// shutdown did not finish cleanly, or nil if it logged none.
func (h *recordingHandler) shutdownError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message != "workqueue server shutdown did not finish cleanly" {
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
