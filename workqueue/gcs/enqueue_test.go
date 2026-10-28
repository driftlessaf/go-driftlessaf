/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"chainguard.dev/driftlessaf/workqueue"
)

// TestQueueRetriesTransientUploadFailure: the storage client retries an upload
// only from its buffered copy of the payload, so a writer with ChunkSize 0
// fails the enqueue on the first 503 instead of retrying it.
func TestQueueRetriesTransientUploadFailure(t *testing.T) {
	var uploads atomic.Int64
	f := &fakeGCS{handler: func(c gcsCall) (int, string) {
		if c.method != http.MethodPost {
			return http.StatusNotFound, errorJSON(http.StatusNotFound)
		}
		if uploads.Add(1) == 1 {
			return http.StatusServiceUnavailable, errorJSON(http.StatusServiceUnavailable)
		}
		return http.StatusOK, objectJSON(queuedPrefix+"key", 1, 1)
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	// Unlike newTestClient, keep the client's default retry policy: that
	// policy is what is under test.
	client, err := storage.NewClient(t.Context(),
		option.WithEndpoint(srv.URL+"/storage/v1/"),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("NewClient() = %v", err)
	}
	t.Cleanup(func() { client.Close() })
	wq := NewWorkQueue(client.Bucket("test-bucket"), 1)

	if err := wq.Queue(t.Context(), "key", workqueue.Options{}); err != nil {
		t.Fatalf("Queue() = %v", err)
	}
	if got, want := uploads.Load(), int64(2); got != want {
		t.Errorf("uploads: got = %d, want = %d", got, want)
	}
}

// TestQueueHoldsNoUploadBufferPerWrite holds concurrent enqueues in flight and
// measures the heap they pin. With the storage client's default ChunkSize each
// writer allocates a 16 MiB upload buffer before its request goes out, so a
// busy ingress needs 16 MiB per concurrent request and is OOM-killed.
func TestQueueHoldsNoUploadBufferPerWrite(t *testing.T) {
	const inFlight = 16

	var arrived atomic.Int64
	allArrived := make(chan struct{})
	release := make(chan struct{})
	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		if arrived.Add(1) == inFlight {
			close(allArrived)
		}
		<-release
		return http.StatusBadRequest, errorJSON(http.StatusBadRequest)
	}}
	wq := NewWorkQueue(newTestClient(t, f), 1)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var wg sync.WaitGroup
	for i := range inFlight {
		wg.Go(func() {
			_ = wq.Queue(t.Context(), fmt.Sprintf("key-%d", i), workqueue.Options{})
		})
	}
	defer wg.Wait()
	// Release the handlers even if the test fails below, so wg.Wait returns.
	defer close(release)

	select {
	case <-allArrived:
	case <-time.After(10 * time.Second):
		t.Fatalf("in-flight enqueues: got = %d, want = %d", arrived.Load(), inFlight)
	}
	var during runtime.MemStats
	runtime.ReadMemStats(&during)

	grown := int64(during.HeapAlloc) - int64(before.HeapAlloc)
	if limit := int64(32 << 20); grown > limit {
		t.Errorf("heap held by %d in-flight enqueues: got = %d MiB, want <= %d MiB", inFlight, grown>>20, limit>>20)
	}
}

// TestUpdateMetadataResetsAttemptsWhenInputCutsBackoffShort: a producer
// enqueue that pulls a pending failure backoff earlier is new input and resets
// the attempt count. Keeping it let a run key whose lifecycle events kept
// arriving retry hot and reach the dead-letter cutoff in seconds. A requeue
// merging into a queued twin is not new input and leaves the count alone.
func TestUpdateMetadataResetsAttemptsWhenInputCutsBackoffShort(t *testing.T) {
	const key = "backing-off"
	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	past := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	tests := []struct {
		name     string
		pending  string
		newInput bool
		// want is the attempt count the update must carry; "" means the
		// count must not be rewritten.
		want string
	}{{
		name:     "enqueue cuts a pending backoff short",
		pending:  future,
		newInput: true,
		want:     "0",
	}, {
		name:     "requeue merging into a twin keeps the count",
		pending:  future,
		newInput: false,
	}, {
		name:     "enqueue after the backoff already elapsed keeps the count",
		pending:  past,
		newInput: true,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGCS{handler: func(call gcsCall) (int, string) {
				return http.StatusOK, fmt.Sprintf(`{"kind":"storage#object","bucket":"test-bucket","name":%q,"generation":"1","metageneration":"1","metadata":{%q:%q,%q:%q,%q:"3"}}`,
					queuedPrefix+key, priorityMetadataKey, formatPriority(100), notBeforeMetadataKey, tc.pending, attemptsMetadataKey)
			}}
			if err := updateMetadata(t.Context(), newTestClient(t, f), key, map[string]string{
				priorityMetadataKey:  formatPriority(100),
				notBeforeMetadataKey: noNotBefore,
			}, tc.newInput); err != nil {
				t.Fatalf("updateMetadata() = %v, want nil", err)
			}
			patch, ok := findCall(f.recorded(), http.MethodPatch, queuedPrefix+key)
			if !ok {
				t.Fatal("PATCH call: got = none, want = one (the not-before is lowered)")
			}
			var body struct {
				Metadata map[string]string `json:"metadata"`
			}
			if err := json.Unmarshal([]byte(patch.body), &body); err != nil {
				t.Fatalf("unmarshaling PATCH body %q = %v", patch.body, err)
			}
			want := cmp.Or(tc.want, "3")
			if got := body.Metadata[attemptsMetadataKey]; got != want {
				t.Errorf("attempts: got = %q, want = %q", got, want)
			}
		})
	}
}

// TestUpdateMetadataRetriesLostRace covers concurrent enqueues of one hot key:
// GCS rejects all but one metadata write with 409 "edited during the
// operation" (or 429 on the per-object mutation rate). The loser re-reads and
// re-merges instead of failing the enqueue.
func TestUpdateMetadataRetriesLostRace(t *testing.T) {
	old := updateMetadataRetryInterval
	updateMetadataRetryInterval = time.Millisecond
	t.Cleanup(func() { updateMetadataRetryInterval = old })

	const key = "hot-key"
	later := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	tests := []struct {
		name string
		// rejection is the status of the first PATCH.
		rejection int
		// winnerApplied makes the re-read after the rejection show the change
		// already in place, as when the winning writer wanted the same thing.
		winnerApplied bool
		wantPatches   int
	}{{
		name:          "409 and the winner already applied the change",
		rejection:     http.StatusConflict,
		winnerApplied: true,
		wantPatches:   1,
	}, {
		name:        "409 and the change still needs writing",
		rejection:   http.StatusConflict,
		wantPatches: 2,
	}, {
		name:        "429 mutation rate",
		rejection:   http.StatusTooManyRequests,
		wantPatches: 2,
	}, {
		name:        "412 precondition",
		rejection:   http.StatusPreconditionFailed,
		wantPatches: 2,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			metageneration, patches, unpinned := int64(1), 0, 0
			notBefore := later
			f := &fakeGCS{}
			f.handler = func(call gcsCall) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				object := func() string {
					return fmt.Sprintf(`{"kind":"storage#object","bucket":"test-bucket","name":%q,"generation":"7","metageneration":"%d","metadata":{%q:%q,%q:%q}}`,
						queuedPrefix+key, metageneration, priorityMetadataKey, formatPriority(100), notBeforeMetadataKey, notBefore)
				}
				switch call.method {
				case http.MethodGet:
					return http.StatusOK, object()
				case http.MethodPatch:
					patches++
					got := call.query.Get("ifMetagenerationMatch")
					if got == "" {
						unpinned++
					} else if got != strconv.FormatInt(metageneration, 10) {
						return http.StatusPreconditionFailed, errorJSON(http.StatusPreconditionFailed)
					}
					if patches == 1 {
						// The winner's write lands as this one is rejected.
						metageneration++
						if tc.winnerApplied {
							notBefore = noNotBefore
						}
						return tc.rejection, errorJSON(tc.rejection)
					}
					metageneration++
					notBefore = noNotBefore
					return http.StatusOK, object()
				}
				return http.StatusInternalServerError, errorJSON(http.StatusInternalServerError)
			}

			if err := updateMetadata(t.Context(), newTestClient(t, f), key, map[string]string{
				priorityMetadataKey:  formatPriority(100),
				notBeforeMetadataKey: noNotBefore,
			}, true); err != nil {
				t.Fatalf("updateMetadata() = %v, want nil", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if patches != tc.wantPatches {
				t.Errorf("PATCH calls: got = %d, want = %d", patches, tc.wantPatches)
			}
			if unpinned != 0 {
				t.Errorf("PATCH calls without ifMetagenerationMatch: got = %d, want = 0", unpinned)
			}
			if notBefore != noNotBefore {
				t.Errorf("queued not-before: got = %q, want = %q", notBefore, noNotBefore)
			}
		})
	}
}
