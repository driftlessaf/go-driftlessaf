/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"chainguard.dev/driftlessaf/store/blob"
)

// versionResponse is one scripted response from the version server. created
// is the RFC 3339 timeCreated for an attributes response, and body is the
// media for a read.
type versionResponse struct {
	status  int
	gen     int64
	created string
	body    string
}

// versionRequest is one request as the version server received it.
type versionRequest struct {
	Kind       string
	Generation string
}

// newVersionServer answers attribute reads and media reads for object from
// two scripts, in order, repeating the last response of each script once it
// runs out. It records every request it receives.
func newVersionServer(t *testing.T, bucket, object string, attrs, media []versionResponse) (*storage.BucketHandle, func() []versionRequest) {
	t.Helper()
	var (
		mu       sync.Mutex
		requests []versionRequest
		calls    = map[string]int{}
	)
	next := func(kind string, script []versionResponse) versionResponse {
		n := calls[kind]
		calls[kind]++
		if n >= len(script) {
			return script[len(script)-1]
		}
		return script[n]
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		gen := r.URL.Query().Get("generation")
		switch r.URL.Path {
		case "/b/" + bucket + "/o/" + object:
			requests = append(requests, versionRequest{Kind: "attrs", Generation: gen})
			resp := next("attrs", attrs)
			w.Header().Set("Content-Type", "application/json")
			if resp.status != http.StatusOK {
				w.WriteHeader(resp.status)
				fmt.Fprintf(w, `{"error":{"code":%d,"message":"attrs failed"}}`, resp.status)
				return
			}
			fmt.Fprintf(w, `{"bucket":%q,"name":%q,"generation":"%d","timeCreated":%q}`, bucket, object, resp.gen, resp.created)
		case "/" + bucket + "/" + object:
			requests = append(requests, versionRequest{Kind: "media", Generation: gen})
			resp := next("media", media)
			if resp.status != http.StatusOK {
				w.WriteHeader(resp.status)
				return
			}
			w.Header().Set("X-Goog-Generation", gen)
			fmt.Fprint(w, resp.body)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	client, err := storage.NewClient(t.Context(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client.Bucket(bucket), func() []versionRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]versionRequest(nil), requests...)
	}
}

// TestGetVersion pins that GetVersion reads the bytes of the generation whose
// attributes it read, starts again when that generation is replaced before
// its bytes are read, and treats a generation without a creation time as an
// error.
func TestGetVersion(t *testing.T) {
	created1 := time.Date(2026, time.November, 19, 20, 0, 0, 123_000_000, time.UTC)
	created2 := created1.Add(time.Minute)
	attrs7 := versionResponse{status: http.StatusOK, gen: 7, created: created1.Format(time.RFC3339Nano)}
	attrs8 := versionResponse{status: http.StatusOK, gen: 8, created: created2.Format(time.RFC3339Nano)}
	notFound := versionResponse{status: http.StatusNotFound}

	for _, tt := range []struct {
		name         string
		attrs, media []versionResponse
		want         blob.Version
		wantOK       bool
		wantErr      bool
		wantStatus   int
		wantRequests []versionRequest
	}{{
		name:         "current generation",
		attrs:        []versionResponse{attrs7},
		media:        []versionResponse{{status: http.StatusOK, body: "first"}},
		want:         blob.Version{Data: []byte("first"), Gen: 7, Created: created1},
		wantOK:       true,
		wantRequests: []versionRequest{{Kind: "attrs"}, {Kind: "media", Generation: "7"}},
	}, {
		name:         "absent object",
		attrs:        []versionResponse{notFound},
		media:        []versionResponse{notFound},
		wantRequests: []versionRequest{{Kind: "attrs"}},
	}, {
		name:         "generation without a creation time",
		attrs:        []versionResponse{{status: http.StatusOK, gen: 7}},
		media:        []versionResponse{{status: http.StatusOK, body: "first"}},
		wantErr:      true,
		wantRequests: []versionRequest{{Kind: "attrs"}},
	}, {
		name:   "replaced before its bytes are read",
		attrs:  []versionResponse{attrs7, attrs8},
		media:  []versionResponse{notFound, {status: http.StatusOK, body: "second"}},
		want:   blob.Version{Data: []byte("second"), Gen: 8, Created: created2},
		wantOK: true,
		wantRequests: []versionRequest{
			{Kind: "attrs"}, {Kind: "media", Generation: "7"},
			{Kind: "attrs"}, {Kind: "media", Generation: "8"},
		},
	}, {
		name:  "deleted before its bytes are read",
		attrs: []versionResponse{attrs7, notFound},
		media: []versionResponse{notFound},
		wantRequests: []versionRequest{
			{Kind: "attrs"}, {Kind: "media", Generation: "7"},
			{Kind: "attrs"},
		},
	}, {
		name:    "replaced before every read",
		attrs:   []versionResponse{attrs7},
		media:   []versionResponse{notFound},
		wantErr: true,
		wantRequests: []versionRequest{
			{Kind: "attrs"}, {Kind: "media", Generation: "7"},
			{Kind: "attrs"}, {Kind: "media", Generation: "7"},
			{Kind: "attrs"}, {Kind: "media", Generation: "7"},
		},
	}, {
		name:         "attributes permission denied",
		attrs:        []versionResponse{{status: http.StatusForbidden}},
		media:        []versionResponse{notFound},
		wantErr:      true,
		wantStatus:   http.StatusForbidden,
		wantRequests: []versionRequest{{Kind: "attrs"}},
	}} {
		t.Run(tt.name, func(t *testing.T) {
			bucket, object := uuid.NewString(), uuid.NewString()
			handle, requests := newVersionServer(t, bucket, object, tt.attrs, tt.media)

			got, ok, err := New(handle).GetVersion(t.Context(), object)
			if (err != nil) != tt.wantErr {
				t.Fatalf("GetVersion error: got = %v, want error = %t", err, tt.wantErr)
			}
			if tt.wantStatus != 0 {
				if gerr, isAPI := errors.AsType[*googleapi.Error](err); !isAPI || gerr.Code != tt.wantStatus {
					t.Errorf("GetVersion error: got = %v, want = HTTP %d", err, tt.wantStatus)
				}
			}
			if ok != tt.wantOK {
				t.Errorf("GetVersion ok: got = %t, want = %t", ok, tt.wantOK)
			}
			if diff := cmp.Diff(tt.want, got, equateBytes); diff != "" {
				t.Errorf("GetVersion (-want, +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantRequests, requests()); diff != "" {
				t.Errorf("requests (-want, +got):\n%s", diff)
			}
		})
	}
}
