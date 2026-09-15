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
	"strconv"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"chainguard.dev/driftlessaf/store/blob"
)

// TestDelete drives the storage client with GCS's observed response for an
// absent object: 404 even when the request includes ifGenerationMatch.
func TestDelete(t *testing.T) {
	for _, tt := range []struct {
		name   string
		gen    blob.Gen
		status int
		want   error
	}{{
		name: "unconditional absent object", status: http.StatusNotFound, want: blob.ErrNotExist,
	}, {
		name: "conditional absent object", gen: 5, status: http.StatusNotFound, want: blob.ErrPreconditionFailed,
	}, {
		name: "stale generation", gen: 5, status: http.StatusPreconditionFailed, want: blob.ErrPreconditionFailed,
	}, {
		name: "matching generation", gen: 5, status: http.StatusNoContent,
	}, {
		name: "conditional permission denied", gen: 5, status: http.StatusForbidden, want: &googleapi.Error{Code: http.StatusForbidden},
	}} {
		t.Run(tt.name, func(t *testing.T) {
			bucket, object := uuid.NewString(), uuid.NewString()
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodDelete || r.URL.Path != "/b/"+bucket+"/o/"+object {
					t.Errorf("request: got = %s %s, want = DELETE /b/%s/o/%s", r.Method, r.URL.Path, bucket, object)
				}
				wantGen := ""
				if tt.gen != 0 {
					wantGen = strconv.FormatInt(int64(tt.gen), 10)
				}
				if got := r.URL.Query().Get("ifGenerationMatch"); got != wantGen {
					t.Errorf("ifGenerationMatch: got = %q, want = %q", got, wantGen)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				if tt.status != http.StatusNoContent {
					fmt.Fprintf(w, `{"error":{"code":%d,"message":"delete failed"}}`, tt.status)
				}
			}))
			defer server.Close()
			client, err := storage.NewClient(t.Context(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			err = New(client.Bucket(bucket)).Delete(t.Context(), object, tt.gen)
			if want, ok := errors.AsType[*googleapi.Error](tt.want); ok {
				got, ok := errors.AsType[*googleapi.Error](err)
				if !ok || got.Code != want.Code {
					t.Errorf("Delete: got = %v, want = HTTP %d", err, want.Code)
				}
			} else if !errors.Is(err, tt.want) {
				t.Errorf("Delete: got = %v, want = %v", err, tt.want)
			}
			if calls != 1 {
				t.Errorf("delete requests: got = %d, want = 1", calls)
			}
		})
	}
}

// TestMapErr pins the translation of real GCS client errors into the blob
// sentinels: not-found (404 / storage.ErrObjectNotExist) → ErrNotExist,
// precondition-failed (412) → ErrPreconditionFailed — including wrapped forms —
// while anything else passes through so callers can retry transient failures.
func TestMapErr(t *testing.T) {
	serverErr := &googleapi.Error{Code: http.StatusServiceUnavailable, Message: "backend unavailable"}
	opaqueErr := errors.New("connection reset")

	tests := []struct {
		name string
		err  error
		want error
	}{{
		name: "nil passes through",
		err:  nil,
		want: nil,
	}, {
		name: "sentinel not-exist maps to ErrNotExist",
		err:  storage.ErrObjectNotExist,
		want: blob.ErrNotExist,
	}, {
		name: "http 404 maps to ErrNotExist",
		err:  &googleapi.Error{Code: http.StatusNotFound},
		want: blob.ErrNotExist,
	}, {
		name: "wrapped 404 maps to ErrNotExist",
		err:  fmt.Errorf("deleting object: %w", &googleapi.Error{Code: http.StatusNotFound}),
		want: blob.ErrNotExist,
	}, {
		name: "http 412 maps to ErrPreconditionFailed",
		err:  &googleapi.Error{Code: http.StatusPreconditionFailed},
		want: blob.ErrPreconditionFailed,
	}, {
		name: "wrapped 412 maps to ErrPreconditionFailed",
		err:  fmt.Errorf("writing object: %w", &googleapi.Error{Code: http.StatusPreconditionFailed}),
		want: blob.ErrPreconditionFailed,
	}, {
		name: "grpc NotFound maps to ErrNotExist",
		err:  status.Error(codes.NotFound, "no such object"),
		want: blob.ErrNotExist,
	}, {
		name: "grpc FailedPrecondition maps to ErrPreconditionFailed",
		err:  status.Error(codes.FailedPrecondition, "generation mismatch"),
		want: blob.ErrPreconditionFailed,
	}, {
		name: "wrapped grpc FailedPrecondition maps to ErrPreconditionFailed",
		err:  fmt.Errorf("writing object: %w", status.Error(codes.FailedPrecondition, "generation mismatch")),
		want: blob.ErrPreconditionFailed,
	}, {
		name: "server error passes through",
		err:  serverErr,
		want: serverErr,
	}, {
		name: "opaque error passes through",
		err:  opaqueErr,
		want: opaqueErr,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapErr(tt.err); !errors.Is(got, tt.want) {
				t.Errorf("mapErr: got = %v, want = %v", got, tt.want)
			}
		})
	}
}

// TestPutErr pins that Put prefers the Close error over the Write error: for an
// async upload, Write can return a pipe-level error while Close reports the
// server's authoritative response (e.g. a 412 → ErrPreconditionFailed), so Close
// must win. Both nil means the upload committed.
func TestPutErr(t *testing.T) {
	writeErr := errors.New("pipe: connection reset")
	server412 := &googleapi.Error{Code: http.StatusPreconditionFailed}

	tests := []struct {
		name             string
		writeErr, cloErr error
		want             error // nil means "no error"
	}{{
		name: "both nil is success",
		want: nil,
	}, {
		name:     "close 412 wins over write pipe error",
		writeErr: writeErr,
		cloErr:   server412,
		want:     blob.ErrPreconditionFailed,
	}, {
		name:     "write error surfaces when close succeeds",
		writeErr: writeErr,
		want:     writeErr,
	}, {
		name:   "close 412 alone",
		cloErr: server412,
		want:   blob.ErrPreconditionFailed,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := putErr("obj", tt.writeErr, tt.cloErr)
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("putErr: got %v, want nil", got)
			case tt.want != nil && !errors.Is(got, tt.want):
				t.Errorf("putErr: got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestConditions pins the blob.Cond → storage.Conditions translation, including
// that the zero Cond yields ok=false so the caller skips Object.If (GCS rejects
// an all-zero Conditions), and that DoesNotExist takes precedence.
func TestConditions(t *testing.T) {
	tests := []struct {
		name   string
		cond   blob.Cond
		want   storage.Conditions
		wantOK bool
	}{{
		name:   "zero cond is unconditional",
		cond:   blob.Cond{},
		wantOK: false,
	}, {
		name:   "does-not-exist",
		cond:   blob.Cond{DoesNotExist: true},
		want:   storage.Conditions{DoesNotExist: true},
		wantOK: true,
	}, {
		name:   "generation match",
		cond:   blob.Cond{GenerationMatch: 42},
		want:   storage.Conditions{GenerationMatch: 42},
		wantOK: true,
	}, {
		name:   "does-not-exist takes precedence over generation match",
		cond:   blob.Cond{DoesNotExist: true, GenerationMatch: 42},
		want:   storage.Conditions{DoesNotExist: true},
		wantOK: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := conditions(tt.cond)
			if ok != tt.wantOK {
				t.Errorf("conditions ok: got = %t, want = %t", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("conditions: got = %+v, want = %+v", got, tt.want)
			}
		})
	}
}
