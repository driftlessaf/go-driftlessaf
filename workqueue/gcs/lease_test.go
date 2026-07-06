/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"chainguard.dev/driftlessaf/workqueue"
)

// gcsCall records a single request the fake GCS endpoint received.
type gcsCall struct {
	method string
	path   string // unescaped URL path
	query  url.Values
}

// fakeGCS is an httptest-backed stand-in for the GCS JSON API that records
// every call and delegates responses to a test-provided handler.
type fakeGCS struct {
	mu      sync.Mutex
	calls   []gcsCall
	handler func(call gcsCall) (status int, body string)
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := gcsCall{method: r.Method, path: r.URL.Path, query: r.URL.Query()}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	handler := f.handler
	f.mu.Unlock()

	status, body := handler(call)
	if body == "" {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func (f *fakeGCS) recorded() []gcsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]gcsCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// findCall returns the first recorded call matching method whose path contains
// pathSubstr.
func findCall(calls []gcsCall, method, pathSubstr string) (gcsCall, bool) {
	for _, c := range calls {
		if c.method == method && strings.Contains(c.path, pathSubstr) {
			return c, true
		}
	}
	return gcsCall{}, false
}

func objectJSON(name string, gen, metagen int64) string {
	return fmt.Sprintf(`{"kind":"storage#object","bucket":"test-bucket","name":%q,"generation":"%d","metageneration":"%d"}`, name, gen, metagen)
}

func rewriteJSON(name string, gen int64) string {
	// A rewrite produces a fresh generation, whose metageneration starts at 1.
	return fmt.Sprintf(`{"kind":"storage#rewriteResponse","totalBytesRewritten":"0","objectSize":"0","done":true,"resource":%s}`, objectJSON(name, gen, 1))
}

func errorJSON(code int) string {
	return fmt.Sprintf(`{"error":{"code":%d,"message":"scripted error"}}`, code)
}

// newTestClient points a storage client at the fake and disables the client's
// internal retries so scripted response sequences are deterministic.
func newTestClient(t *testing.T, f *fakeGCS) ClientInterface {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	client, err := storage.NewClient(t.Context(),
		option.WithEndpoint(srv.URL+"/storage/v1/"),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("NewClient() = %v", err)
	}
	t.Cleanup(func() { client.Close() })
	client.SetRetry(storage.WithPolicy(storage.RetryNever))
	return client.Bucket("test-bucket")
}

// newTestKey builds an inProgressKey as Start would have, owned by a
// cancelable context so cleanup paths that invoke ownerCancel are safe to
// call.
func newTestKey(t *testing.T, client ClientInterface, key string, gen, metagen int64) *inProgressKey {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return &inProgressKey{
		client:      client,
		ownerCtx:    ctx,
		ownerCancel: cancel,
		attrs: &storage.ObjectAttrs{
			Name:           inProgressPrefix + key,
			Generation:     gen,
			Metageneration: metagen,
			Metadata: map[string]string{
				attemptsMetadataKey: "1",
				priorityMetadataKey: noPriority,
			},
		},
	}
}

// newObservedKey builds an inProgressKey as Enumerate would have: no owner
// context and no heartbeat.
func newObservedKey(client ClientInterface, key string, gen, metagen int64) *inProgressKey {
	return &inProgressKey{
		client: client,
		attrs: &storage.ObjectAttrs{
			Name:           inProgressPrefix + key,
			Generation:     gen,
			Metageneration: metagen,
			Metadata: map[string]string{
				attemptsMetadataKey: "1",
				priorityMetadataKey: noPriority,
			},
		},
	}
}

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// setLeaseIntervals shortens the heartbeat timers for a test and restores them
// afterward. Tests using this must not run in parallel.
func setLeaseIntervals(t *testing.T, refresh time.Duration) {
	t.Helper()
	origRefresh, origRetry := RefreshInterval, heartbeatRetryInterval
	RefreshInterval, heartbeatRetryInterval = refresh, 20*time.Millisecond
	t.Cleanup(func() {
		RefreshInterval, heartbeatRetryInterval = origRefresh, origRetry
	})
}

// startHeartbeatForTest starts the heartbeat and guarantees the goroutine has
// fully exited before the test's interval globals are restored.
func startHeartbeatForTest(t *testing.T, oip *inProgressKey) {
	t.Helper()
	oip.startHeartbeat(t.Context())
	t.Cleanup(func() {
		oip.ownerCancel()
		<-oip.heartbeatStopped
	})
}

func TestIsNotFoundAndLostOwnership(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantNotFound  bool
		wantOwnership bool
	}{{
		name: "nil error",
	}, {
		name:          "precondition failed",
		err:           &googleapi.Error{Code: http.StatusPreconditionFailed},
		wantOwnership: true,
	}, {
		name:          "wrapped precondition failed",
		err:           fmt.Errorf("Run() = %w", &googleapi.Error{Code: http.StatusPreconditionFailed}),
		wantOwnership: true,
	}, {
		name:          "not found",
		err:           &googleapi.Error{Code: http.StatusNotFound},
		wantNotFound:  true,
		wantOwnership: true,
	}, {
		name:          "object does not exist",
		err:           storage.ErrObjectNotExist,
		wantNotFound:  true,
		wantOwnership: true,
	}, {
		name:          "wrapped object does not exist",
		err:           fmt.Errorf("deleting: %w", storage.ErrObjectNotExist),
		wantNotFound:  true,
		wantOwnership: true,
	}, {
		name: "rate limited",
		err:  &googleapi.Error{Code: http.StatusTooManyRequests},
	}, {
		name: "server error",
		err:  &googleapi.Error{Code: http.StatusServiceUnavailable},
	}, {
		name: "generic error",
		err:  errors.New("boom"),
	}, {
		name: "context deadline",
		err:  context.DeadlineExceeded,
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isNotFound(test.err); got != test.wantNotFound {
				t.Errorf("isNotFound: got = %v, want = %v", got, test.wantNotFound)
			}
			if got := lostOwnership(test.err); got != test.wantOwnership {
				t.Errorf("lostOwnership: got = %v, want = %v", got, test.wantOwnership)
			}
		})
	}
}

func TestHeartbeatExtendsLeaseWithGenerationPins(t *testing.T) {
	setLeaseIntervals(t, 30*time.Millisecond)

	key := fmt.Sprintf("test-%d", rand.Int64())
	gen, metagen := rand.Int64N(1<<40)+1, rand.Int64N(100)+1

	var mu sync.Mutex
	serverMetagen := metagen
	f := &fakeGCS{}
	f.handler = func(call gcsCall) (int, string) {
		// Bump the metageneration server-side, as a real metadata update would.
		mu.Lock()
		serverMetagen++
		mg := serverMetagen
		mu.Unlock()
		return http.StatusOK, objectJSON(inProgressPrefix+key, gen, mg)
	}
	oip := newTestKey(t, newTestClient(t, f), key, gen, metagen)

	startHeartbeatForTest(t, oip)

	waitFor(t, func() bool {
		return len(f.recorded()) >= 2
	}, "two heartbeat updates")

	if err := oip.Context().Err(); err != nil {
		t.Errorf("owner context: got = %v, want = nil", err)
	}
	for i, call := range f.recorded()[:2] {
		if got := call.query.Get("ifGenerationMatch"); got != strconv.FormatInt(gen, 10) {
			t.Errorf("call %d ifGenerationMatch: got = %q, want = %q", i, got, strconv.FormatInt(gen, 10))
		}
		// The metageneration is deliberately unpinned: our own refresh can
		// land server-side while failing client-side, so pinning it would
		// 412 against our own write.
		if got := call.query.Get("ifMetagenerationMatch"); got != "" {
			t.Errorf("call %d ifMetagenerationMatch: got = %q, want = unset", i, got)
		}
	}
}

func TestHeartbeatCancelsOnLostOwnership(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{{
		name:   "precondition failed",
		status: http.StatusPreconditionFailed,
	}, {
		name:   "not found",
		status: http.StatusNotFound,
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setLeaseIntervals(t, 30*time.Millisecond)

			key := fmt.Sprintf("test-%d", rand.Int64())
			f := &fakeGCS{}
			f.handler = func(gcsCall) (int, string) {
				return test.status, errorJSON(test.status)
			}
			oip := newTestKey(t, newTestClient(t, f), key, rand.Int64N(1<<40)+1, 1)

			startHeartbeatForTest(t, oip)

			waitFor(t, func() bool {
				return oip.Context().Err() != nil
			}, "owner context cancellation")

			if got := len(f.recorded()); got != 1 {
				t.Errorf("update attempts: got = %d, want = 1 (no retry on definitive loss)", got)
			}
		})
	}
}

func TestHeartbeatRetriesTransientErrors(t *testing.T) {
	setLeaseIntervals(t, 100*time.Millisecond)

	key := fmt.Sprintf("test-%d", rand.Int64())
	gen := rand.Int64N(1<<40) + 1

	var mu sync.Mutex
	failures := 2
	f := &fakeGCS{}
	f.handler = func(call gcsCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if failures > 0 {
			failures--
			return http.StatusServiceUnavailable, errorJSON(http.StatusServiceUnavailable)
		}
		mg, _ := strconv.ParseInt(call.query.Get("ifMetagenerationMatch"), 10, 64)
		return http.StatusOK, objectJSON(inProgressPrefix+key, gen, mg+1)
	}
	oip := newTestKey(t, newTestClient(t, f), key, gen, 1)

	startHeartbeatForTest(t, oip)

	// Two transient failures then a success: the lease survives.
	waitFor(t, func() bool {
		return len(f.recorded()) >= 3
	}, "retried heartbeat updates")

	if err := oip.Context().Err(); err != nil {
		t.Errorf("owner context: got = %v, want = nil (transient errors must not cancel)", err)
	}
}

func TestHeartbeatGivesUpAtLeaseExpiry(t *testing.T) {
	setLeaseIntervals(t, 50*time.Millisecond)

	key := fmt.Sprintf("test-%d", rand.Int64())
	f := &fakeGCS{}
	f.handler = func(gcsCall) (int, string) {
		return http.StatusServiceUnavailable, errorJSON(http.StatusServiceUnavailable)
	}
	oip := newTestKey(t, newTestClient(t, f), key, rand.Int64N(1<<40)+1, 1)

	startHeartbeatForTest(t, oip)

	waitFor(t, func() bool {
		return oip.Context().Err() != nil
	}, "owner context cancellation at lease expiry")

	if got := len(f.recorded()); got < 2 {
		t.Errorf("update attempts: got = %d, want >= 2 (transient errors retry before giving up)", got)
	}
}

func TestRequeuePinsGenerations(t *testing.T) {
	key := fmt.Sprintf("test-%d", rand.Int64())
	gen := rand.Int64N(1<<40) + 1

	f := &fakeGCS{}
	f.handler = func(call gcsCall) (int, string) {
		switch {
		case call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/"):
			return http.StatusOK, rewriteJSON(queuedPrefix+key, rand.Int64N(1<<40)+1)
		case call.method == http.MethodDelete:
			return http.StatusNoContent, ""
		}
		return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
	}
	oip := newTestKey(t, newTestClient(t, f), key, gen, 1)

	if err := oip.RequeueWithOptions(t.Context(), workqueue.Options{}); err != nil {
		t.Fatalf("RequeueWithOptions() = %v, want nil", err)
	}

	calls := f.recorded()
	rewrite, ok := findCall(calls, http.MethodPost, "/rewriteTo/")
	if !ok {
		t.Fatal("rewrite call: got = none, want = one")
	}
	if got := rewrite.query.Get("sourceGeneration"); got != strconv.FormatInt(gen, 10) {
		t.Errorf("rewrite sourceGeneration: got = %q, want = %q", got, strconv.FormatInt(gen, 10))
	}
	if got := rewrite.query.Get("ifGenerationMatch"); got != "0" {
		t.Errorf("rewrite destination ifGenerationMatch: got = %q, want = %q (DoesNotExist)", got, "0")
	}
	del, ok := findCall(calls, http.MethodDelete, "/o/"+inProgressPrefix+key)
	if !ok {
		t.Fatal("in-progress delete call: got = none, want = one")
	}
	if got := del.query.Get("ifGenerationMatch"); got != strconv.FormatInt(gen, 10) {
		t.Errorf("delete ifGenerationMatch: got = %q, want = %q", got, strconv.FormatInt(gen, 10))
	}
}

func TestRequeueSkipsWhenOwnershipLost(t *testing.T) {
	t.Run("source generation gone", func(t *testing.T) {
		key := fmt.Sprintf("test-%d", rand.Int64())
		f := &fakeGCS{}
		f.handler = func(call gcsCall) (int, string) {
			if call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/") {
				return http.StatusNotFound, errorJSON(http.StatusNotFound)
			}
			return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
		}
		oip := newTestKey(t, newTestClient(t, f), key, rand.Int64N(1<<40)+1, 1)

		if err := oip.RequeueWithOptions(t.Context(), workqueue.Options{}); err != nil {
			t.Fatalf("RequeueWithOptions() = %v, want nil", err)
		}
		if _, ok := findCall(f.recorded(), http.MethodDelete, "/o/"); ok {
			t.Error("delete call: got = one, want = none (must not touch the new owner's object)")
		}
	})

	t.Run("replaced between copy and delete", func(t *testing.T) {
		key := fmt.Sprintf("test-%d", rand.Int64())
		twinGen := rand.Int64N(1<<40) + 1
		f := &fakeGCS{}
		f.handler = func(call gcsCall) (int, string) {
			switch {
			case call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/"):
				return http.StatusOK, rewriteJSON(queuedPrefix+key, twinGen)
			case call.method == http.MethodDelete && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
				return http.StatusPreconditionFailed, errorJSON(http.StatusPreconditionFailed)
			case call.method == http.MethodDelete && strings.Contains(call.path, "/o/"+queuedPrefix+key):
				return http.StatusNoContent, ""
			}
			return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
		}
		oip := newTestKey(t, newTestClient(t, f), key, rand.Int64N(1<<40)+1, 1)

		if err := oip.RequeueWithOptions(t.Context(), workqueue.Options{}); err != nil {
			t.Fatalf("RequeueWithOptions() = %v, want nil", err)
		}
		// A live attempt still holds the in-progress object (its owner refreshed
		// the lease), so the twin is LEFT intact, never deleted: a concurrent
		// enqueue may have deduped into it without bumping its metageneration, so
		// deleting it could drop that event. A duplicate re-run is the safe bias.
		if _, ok := findCall(f.recorded(), http.MethodDelete, "/o/"+queuedPrefix+key); ok {
			t.Error("queued twin delete call: got = one, want = none (deleting could drop a deduped enqueue; leave it for at-least-once)")
		}
	})

	t.Run("keeps twin when in-progress already gone", func(t *testing.T) {
		key := fmt.Sprintf("test-%d", rand.Int64())
		f := &fakeGCS{}
		f.handler = func(call gcsCall) (int, string) {
			switch {
			case call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/"):
				return http.StatusOK, rewriteJSON(queuedPrefix+key, rand.Int64N(1<<40)+1)
			case call.method == http.MethodDelete && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
				return http.StatusNotFound, errorJSON(http.StatusNotFound)
			}
			return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
		}
		oip := newTestKey(t, newTestClient(t, f), key, rand.Int64N(1<<40)+1, 1)

		if err := oip.RequeueWithOptions(t.Context(), workqueue.Options{}); err != nil {
			t.Fatalf("RequeueWithOptions() = %v, want nil", err)
		}
		// With the in-progress object gone, the queued copy is the key's only
		// remaining record and must be left in place.
		if _, ok := findCall(f.recorded(), http.MethodDelete, "/o/"+queuedPrefix+key); ok {
			t.Error("queued twin delete call: got = one, want = none (the twin is the key's only record)")
		}
	})
}

func TestRequeueMergesIntoExistingQueuedTwin(t *testing.T) {
	key := fmt.Sprintf("test-%d", rand.Int64())
	gen := rand.Int64N(1<<40) + 1

	f := &fakeGCS{}
	f.handler = func(call gcsCall) (int, string) {
		switch {
		case call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/"):
			// The queued twin already exists.
			return http.StatusPreconditionFailed, errorJSON(http.StatusPreconditionFailed)
		case call.method == http.MethodGet && strings.Contains(call.path, "/o/"+queuedPrefix+key):
			return http.StatusOK, objectJSON(queuedPrefix+key, rand.Int64N(1<<40)+1, 1)
		case call.method == http.MethodPatch && strings.Contains(call.path, "/o/"+queuedPrefix+key):
			return http.StatusOK, objectJSON(queuedPrefix+key, rand.Int64N(1<<40)+1, 2)
		case call.method == http.MethodDelete:
			return http.StatusNoContent, ""
		}
		return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
	}
	oip := newTestKey(t, newTestClient(t, f), key, gen, 1)

	if err := oip.RequeueWithOptions(t.Context(), workqueue.Options{}); err != nil {
		t.Fatalf("RequeueWithOptions() = %v, want nil", err)
	}
	del, ok := findCall(f.recorded(), http.MethodDelete, "/o/"+inProgressPrefix+key)
	if !ok {
		t.Fatal("in-progress delete call: got = none, want = one")
	}
	if got := del.query.Get("ifGenerationMatch"); got != strconv.FormatInt(gen, 10) {
		t.Errorf("delete ifGenerationMatch: got = %q, want = %q", got, strconv.FormatInt(gen, 10))
	}
}

func TestCompletePinsDelete(t *testing.T) {
	tests := []struct {
		name         string
		deleteStatus int
		wantErr      bool
	}{{
		name:         "deletes own generation",
		deleteStatus: http.StatusNoContent,
	}, {
		name:         "skips delete when ownership lost",
		deleteStatus: http.StatusPreconditionFailed,
	}, {
		name:         "surfaces transient errors",
		deleteStatus: http.StatusServiceUnavailable,
		wantErr:      true,
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key := fmt.Sprintf("test-%d", rand.Int64())
			gen := rand.Int64N(1<<40) + 1

			f := &fakeGCS{}
			f.handler = func(call gcsCall) (int, string) {
				switch {
				case call.method == http.MethodDelete && strings.Contains(call.path, "/o/"+deadLetterPrefix+key):
					// Best-effort dead-letter cleanup: nothing to delete.
					return http.StatusNotFound, errorJSON(http.StatusNotFound)
				case call.method == http.MethodDelete && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
					if test.deleteStatus == http.StatusNoContent {
						return http.StatusNoContent, ""
					}
					return test.deleteStatus, errorJSON(test.deleteStatus)
				}
				return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
			}
			oip := newTestKey(t, newTestClient(t, f), key, gen, 1)

			err := oip.Complete(t.Context())
			if gotErr := err != nil; gotErr != test.wantErr {
				t.Fatalf("Complete() = %v, want error = %v", err, test.wantErr)
			}
			del, ok := findCall(f.recorded(), http.MethodDelete, "/o/"+inProgressPrefix+key)
			if !ok {
				t.Fatal("in-progress delete call: got = none, want = one")
			}
			if got := del.query.Get("ifGenerationMatch"); got != strconv.FormatInt(gen, 10) {
				t.Errorf("delete ifGenerationMatch: got = %q, want = %q", got, strconv.FormatInt(gen, 10))
			}
		})
	}
}

func TestDeadletterPinsGenerations(t *testing.T) {
	t.Run("dead-letters own generation", func(t *testing.T) {
		key := fmt.Sprintf("test-%d", rand.Int64())
		gen := rand.Int64N(1<<40) + 1

		f := &fakeGCS{}
		f.handler = func(call gcsCall) (int, string) {
			switch {
			case call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/"):
				return http.StatusOK, rewriteJSON(deadLetterPrefix+key, rand.Int64N(1<<40)+1)
			case call.method == http.MethodDelete:
				return http.StatusNoContent, ""
			}
			return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
		}
		oip := newTestKey(t, newTestClient(t, f), key, gen, 1)

		if err := oip.Deadletter(t.Context()); err != nil {
			t.Fatalf("Deadletter() = %v, want nil", err)
		}
		calls := f.recorded()
		rewrite, ok := findCall(calls, http.MethodPost, "/rewriteTo/")
		if !ok {
			t.Fatal("rewrite call: got = none, want = one")
		}
		if got := rewrite.query.Get("sourceGeneration"); got != strconv.FormatInt(gen, 10) {
			t.Errorf("rewrite sourceGeneration: got = %q, want = %q", got, strconv.FormatInt(gen, 10))
		}
		del, ok := findCall(calls, http.MethodDelete, "/o/"+inProgressPrefix+key)
		if !ok {
			t.Fatal("in-progress delete call: got = none, want = one")
		}
		if got := del.query.Get("ifGenerationMatch"); got != strconv.FormatInt(gen, 10) {
			t.Errorf("delete ifGenerationMatch: got = %q, want = %q", got, strconv.FormatInt(gen, 10))
		}
	})

	t.Run("skips when source generation gone", func(t *testing.T) {
		key := fmt.Sprintf("test-%d", rand.Int64())
		f := &fakeGCS{}
		f.handler = func(call gcsCall) (int, string) {
			if call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/") {
				return http.StatusNotFound, errorJSON(http.StatusNotFound)
			}
			return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
		}
		oip := newTestKey(t, newTestClient(t, f), key, rand.Int64N(1<<40)+1, 1)

		if err := oip.Deadletter(t.Context()); err != nil {
			t.Fatalf("Deadletter() = %v, want nil", err)
		}
		if _, ok := findCall(f.recorded(), http.MethodDelete, "/o/"); ok {
			t.Error("delete call: got = one, want = none (must not touch the new owner's object)")
		}
	})

	t.Run("skips delete when replaced after copy", func(t *testing.T) {
		key := fmt.Sprintf("test-%d", rand.Int64())
		f := &fakeGCS{}
		f.handler = func(call gcsCall) (int, string) {
			switch {
			case call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/"):
				return http.StatusOK, rewriteJSON(deadLetterPrefix+key, rand.Int64N(1<<40)+1)
			case call.method == http.MethodDelete:
				return http.StatusPreconditionFailed, errorJSON(http.StatusPreconditionFailed)
			}
			return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
		}
		oip := newTestKey(t, newTestClient(t, f), key, rand.Int64N(1<<40)+1, 1)

		if err := oip.Deadletter(t.Context()); err != nil {
			t.Fatalf("Deadletter() = %v, want nil", err)
		}
	})
}

func TestObservedRequeueFreshnessGuard(t *testing.T) {
	tests := []struct {
		name        string
		attrsStatus func(gen, metagen int64) (int, string)
		wantRequeue bool
	}{{
		name: "requeues when lease unchanged",
		attrsStatus: func(gen, metagen int64) (int, string) {
			return http.StatusOK, ""
		},
		wantRequeue: true,
	}, {
		name: "skips when lease refreshed since observation",
		attrsStatus: func(gen, metagen int64) (int, string) {
			return http.StatusOK, "bump-metagen"
		},
	}, {
		name: "skips when object replaced",
		attrsStatus: func(gen, metagen int64) (int, string) {
			return http.StatusOK, "bump-gen"
		},
	}, {
		name: "skips when object gone",
		attrsStatus: func(gen, metagen int64) (int, string) {
			return http.StatusNotFound, errorJSON(http.StatusNotFound)
		},
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key := fmt.Sprintf("test-%d", rand.Int64())
			gen, metagen := rand.Int64N(1<<40)+1, rand.Int64N(100)+2

			f := &fakeGCS{}
			f.handler = func(call gcsCall) (int, string) {
				switch {
				case call.method == http.MethodGet && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
					status, body := test.attrsStatus(gen, metagen)
					if status != http.StatusOK {
						return status, body
					}
					switch body {
					case "bump-metagen":
						return status, objectJSON(inProgressPrefix+key, gen, metagen+1)
					case "bump-gen":
						return status, objectJSON(inProgressPrefix+key, gen+1, 1)
					}
					return status, objectJSON(inProgressPrefix+key, gen, metagen)
				case call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/"):
					return http.StatusOK, rewriteJSON(queuedPrefix+key, rand.Int64N(1<<40)+1)
				case call.method == http.MethodDelete:
					return http.StatusNoContent, ""
				}
				return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
			}
			oip := newObservedKey(newTestClient(t, f), key, gen, metagen)

			if err := oip.RequeueWithOptions(t.Context(), workqueue.Options{}); err != nil {
				t.Fatalf("RequeueWithOptions() = %v, want nil", err)
			}

			calls := f.recorded()
			_, gotRewrite := findCall(calls, http.MethodPost, "/rewriteTo/")
			if gotRewrite != test.wantRequeue {
				t.Errorf("rewrite call present: got = %v, want = %v", gotRewrite, test.wantRequeue)
			}
			del, gotDelete := findCall(calls, http.MethodDelete, "/o/"+inProgressPrefix+key)
			if gotDelete != test.wantRequeue {
				t.Errorf("delete call present: got = %v, want = %v", gotDelete, test.wantRequeue)
			}
			if !test.wantRequeue {
				return
			}
			// The observed-key delete must pin both the generation and the
			// metageneration verified by the freshness check.
			if got := del.query.Get("ifGenerationMatch"); got != strconv.FormatInt(gen, 10) {
				t.Errorf("delete ifGenerationMatch: got = %q, want = %q", got, strconv.FormatInt(gen, 10))
			}
			if got := del.query.Get("ifMetagenerationMatch"); got != strconv.FormatInt(metagen, 10) {
				t.Errorf("delete ifMetagenerationMatch: got = %q, want = %q", got, strconv.FormatInt(metagen, 10))
			}
		})
	}
}

func TestCompleteDrainsActiveHeartbeat(t *testing.T) {
	setLeaseIntervals(t, 30*time.Millisecond)

	key := fmt.Sprintf("test-%d", rand.Int64())
	gen := rand.Int64N(1<<40) + 1

	f := &fakeGCS{}
	f.handler = func(call gcsCall) (int, string) {
		switch {
		case call.method == http.MethodPatch:
			// Hold heartbeat updates open so Complete races an in-flight
			// refresh; cancellation must abort it client-side.
			time.Sleep(100 * time.Millisecond)
			mg, _ := strconv.ParseInt(call.query.Get("ifMetagenerationMatch"), 10, 64)
			return http.StatusOK, objectJSON(inProgressPrefix+key, gen, mg+1)
		case call.method == http.MethodDelete && strings.Contains(call.path, "/o/"+deadLetterPrefix+key):
			return http.StatusNotFound, errorJSON(http.StatusNotFound)
		case call.method == http.MethodDelete && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
			return http.StatusNoContent, ""
		}
		return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
	}
	oip := newTestKey(t, newTestClient(t, f), key, gen, 1)

	startHeartbeatForTest(t, oip)

	// Wait until at least one heartbeat update is in flight.
	waitFor(t, func() bool {
		_, ok := findCall(f.recorded(), http.MethodPatch, "/o/"+inProgressPrefix+key)
		return ok
	}, "an in-flight heartbeat update")

	if err := oip.Complete(t.Context()); err != nil {
		t.Fatalf("Complete() = %v, want nil", err)
	}

	// Complete must have waited for the heartbeat goroutine to exit.
	select {
	case <-oip.heartbeatStopped:
	default:
		t.Error("heartbeat goroutine: got = running, want = stopped after Complete")
	}
}

func TestRequeueRetriesWhenQueuedTwinVanishesMidMerge(t *testing.T) {
	key := fmt.Sprintf("test-%d", rand.Int64())
	gen := rand.Int64N(1<<40) + 1

	// First pass: the copy hits the existing queued twin (precondition
	// failure), but the twin is deleted before its metadata can be merged.
	// Second pass: the copy succeeds against the now-vacant queued name.
	var mu sync.Mutex
	rewrites := 0
	f := &fakeGCS{}
	f.handler = func(call gcsCall) (int, string) {
		switch {
		case call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/"):
			mu.Lock()
			defer mu.Unlock()
			rewrites++
			if rewrites == 1 {
				return http.StatusPreconditionFailed, errorJSON(http.StatusPreconditionFailed)
			}
			return http.StatusOK, rewriteJSON(queuedPrefix+key, rand.Int64N(1<<40)+1)
		case call.method == http.MethodGet && strings.Contains(call.path, "/o/"+queuedPrefix+key):
			return http.StatusNotFound, errorJSON(http.StatusNotFound)
		case call.method == http.MethodDelete && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
			return http.StatusNoContent, ""
		}
		return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
	}
	oip := newTestKey(t, newTestClient(t, f), key, gen, 1)

	if err := oip.RequeueWithOptions(t.Context(), workqueue.Options{}); err != nil {
		t.Fatalf("RequeueWithOptions() = %v, want nil", err)
	}

	calls := f.recorded()
	gotRewrites := 0
	for _, c := range calls {
		if c.method == http.MethodPost && strings.Contains(c.path, "/rewriteTo/") {
			gotRewrites++
		}
	}
	if gotRewrites != 2 {
		t.Errorf("rewrite attempts: got = %d, want = 2 (one merge collision, one retry)", gotRewrites)
	}
	del, ok := findCall(calls, http.MethodDelete, "/o/"+inProgressPrefix+key)
	if !ok {
		t.Fatal("in-progress delete call: got = none, want = one")
	}
	if got := del.query.Get("ifGenerationMatch"); got != strconv.FormatInt(gen, 10) {
		t.Errorf("delete ifGenerationMatch: got = %q, want = %q", got, strconv.FormatInt(gen, 10))
	}
}

// TestHeartbeatSurvivesLandedButTimedOutRefresh covers a lease refresh that
// times out client-side (per-attempt timeout) but LANDS server-side, bumping
// the metageneration while the owner's recorded value stays stale. The
// heartbeat's retry must still succeed: it pins only the generation, so a
// stale metageneration can never 412 against the owner's own write and cancel
// healthy work. No other actor touches the object in this scenario.
//
// The fake models real GCS: it evaluates the request's preconditions against
// server-side state and applies the update (bumping the metageneration)
// before the response is delayed past the per-attempt timeout.
func TestHeartbeatSurvivesLandedButTimedOutRefresh(t *testing.T) {
	// Generous intervals so the sequencing is deterministic under -race:
	// per-attempt timeout 60ms, server response delay 150ms, lease 600ms.
	origRefresh, origRetry := RefreshInterval, heartbeatRetryInterval
	RefreshInterval, heartbeatRetryInterval = 200*time.Millisecond, 60*time.Millisecond
	t.Cleanup(func() {
		RefreshInterval, heartbeatRetryInterval = origRefresh, origRetry
	})

	key := fmt.Sprintf("test-%d", rand.Int64())
	gen := rand.Int64N(1<<40) + 1

	var mu sync.Mutex
	metagen := int64(5)
	patches := 0
	f := &fakeGCS{}
	f.handler = func(call gcsCall) (int, string) {
		if call.method != http.MethodPatch {
			return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
		}
		mu.Lock()
		if g, _ := strconv.ParseInt(call.query.Get("ifGenerationMatch"), 10, 64); g != 0 && g != gen {
			mu.Unlock()
			return http.StatusPreconditionFailed, errorJSON(http.StatusPreconditionFailed)
		}
		if mg, _ := strconv.ParseInt(call.query.Get("ifMetagenerationMatch"), 10, 64); mg != 0 && mg != metagen {
			mu.Unlock()
			return http.StatusPreconditionFailed, errorJSON(http.StatusPreconditionFailed)
		}
		// Preconditions hold: the write LANDS.
		metagen++
		patches++
		n, mg := patches, metagen
		mu.Unlock()
		if n == 1 {
			// The first refresh lands server-side, but the response arrives
			// after the heartbeat's per-attempt timeout.
			time.Sleep(150 * time.Millisecond)
		}
		return http.StatusOK, objectJSON(inProgressPrefix+key, gen, mg)
	}
	oip := newTestKey(t, newTestClient(t, f), key, gen, 5)

	startHeartbeatForTest(t, oip)

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return patches >= 1
	}, "first (landed but slow) heartbeat update")

	// Give the heartbeat time to run its retry against the now-stale
	// recorded metageneration.
	time.Sleep(300 * time.Millisecond)

	if err := oip.Context().Err(); err != nil {
		t.Errorf("owner context after landed-but-timed-out refresh: got = %v, want = nil (a stale recorded metageneration must not cancel healthy work)", err)
	}
}

// TestObservedRequeueRemovesAbandonedTwin covers an orphan requeue of an
// Enumerate-observed key that passes the freshness re-read, creates the
// queued twin, and THEN loses the pinned delete because the owner's refresh
// landed in between. The requeue is abandoned, and it must remove the twin it
// created (pinned to the twin's creation generation): a leaked twin would
// re-execute the key as soon as the still-live owner Completes.
// TestObservedRequeueLeavesTwinOnLostOwnership pins the at-least-once bias: when
// an observed requeue creates the queued twin but then loses the pinned
// in-progress delete (the owner refreshed its lease in between), the twin is
// LEFT in place, never deleted. A concurrent enqueue may have deduplicated into
// the twin without bumping its metageneration (dedups are deliberately cheap
// no-ops), so a pristine twin is indistinguishable from one now carrying a real
// queued event; deleting it could drop that event. A spurious re-execution once
// the live owner completes is the safe trade — receivers are idempotent.
func TestObservedRequeueLeavesTwinOnLostOwnership(t *testing.T) {
	key := fmt.Sprintf("test-%d", rand.Int64())
	gen, metagen := rand.Int64N(1<<40)+1, rand.Int64N(100)+2

	var mu sync.Mutex
	twinCreated, twinDeleted := false, false
	f := &fakeGCS{}
	f.handler = func(call gcsCall) (int, string) {
		switch {
		case call.method == http.MethodGet && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
			// Freshness re-read: the lease looks exactly as observed.
			return http.StatusOK, objectJSON(inProgressPrefix+key, gen, metagen)
		case call.method == http.MethodPost && strings.Contains(call.path, "/rewriteTo/"):
			mu.Lock()
			twinCreated = true
			mu.Unlock()
			return http.StatusOK, rewriteJSON(queuedPrefix+key, rand.Int64N(1<<40)+1)
		case call.method == http.MethodDelete && strings.Contains(call.path, "/o/"+inProgressPrefix+key):
			// The owner's lease refresh landed between the freshness re-read
			// and this pinned delete: the metageneration no longer matches.
			return http.StatusPreconditionFailed, errorJSON(http.StatusPreconditionFailed)
		case call.method == http.MethodDelete && strings.Contains(call.path, "/o/"+queuedPrefix+key):
			mu.Lock()
			twinDeleted = true
			mu.Unlock()
			return http.StatusNoContent, ""
		}
		return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
	}
	oip := newObservedKey(newTestClient(t, f), key, gen, metagen)

	if err := oip.RequeueWithOptions(t.Context(), workqueue.Options{}); err != nil {
		t.Fatalf("RequeueWithOptions() = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !twinCreated {
		t.Fatal("queued twin: got = not created, want = created (test scenario did not reach the copy)")
	}
	if twinDeleted {
		t.Error("requeue twin: got = deleted, want = left intact (deleting it could drop an enqueue that deduped into it; a duplicate re-run is the safe bias)")
	}
}
