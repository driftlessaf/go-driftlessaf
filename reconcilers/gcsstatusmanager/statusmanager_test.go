/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstatusmanager

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/api/option"
)

// testDetails is a simple type used as the Details generic parameter in tests.
type testDetails struct {
	Message string `json:"message,omitempty"`
	Count   int    `json:"count,omitempty"`
}

func TestObservedState_NotFound(t *testing.T) {
	client := newTestClient(t)

	m := New[testDetails]("test-identity", client.Bucket("test-bucket"))
	session := mustNewSession(t, m, "images/foo")

	status, err := session.ObservedState(t.Context())
	if err != nil {
		t.Fatalf("ObservedState() error = %v", err)
	}
	if status != nil {
		t.Fatalf("ObservedState() = %v, want nil", status)
	}
}

func TestSetAndObserveState(t *testing.T) {
	client := newTestClient(t)
	ctx := t.Context()

	m := New[testDetails]("test-identity", client.Bucket("test-bucket"))
	session := mustNewSession(t, m, "images/foo")

	want := &Status[testDetails]{
		ObservedGeneration: "abc123",
		Details: testDetails{
			Message: "dispatched",
			Count:   1,
		},
	}

	if err := session.SetActualState(ctx, want); err != nil {
		t.Fatalf("SetActualState() error = %v", err)
	}

	got, err := session.ObservedState(ctx)
	if err != nil {
		t.Fatalf("ObservedState() error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ObservedState() mismatch (-want +got):\n%s", diff)
	}
}

func TestSetActualState_Overwrite(t *testing.T) {
	client := newTestClient(t)
	ctx := t.Context()

	m := New[testDetails]("test-identity", client.Bucket("test-bucket"))
	session := mustNewSession(t, m, "images/bar")

	first := &Status[testDetails]{
		ObservedGeneration: "sha1",
		Details:            testDetails{Message: "first"},
	}
	if err := session.SetActualState(ctx, first); err != nil {
		t.Fatalf("SetActualState(first) error = %v", err)
	}

	second := &Status[testDetails]{
		ObservedGeneration: "sha2",
		Details:            testDetails{Message: "second", Count: 42},
	}
	if err := session.SetActualState(ctx, second); err != nil {
		t.Fatalf("SetActualState(second) error = %v", err)
	}

	got, err := session.ObservedState(ctx)
	if err != nil {
		t.Fatalf("ObservedState() error = %v", err)
	}
	if diff := cmp.Diff(second, got); diff != "" {
		t.Errorf("ObservedState() mismatch (-want +got):\n%s", diff)
	}
}

func TestReadOnly(t *testing.T) {
	client := newTestClient(t)
	ctx := t.Context()

	// Write with a writable manager first.
	writable := New[testDetails]("test-identity", client.Bucket("test-bucket"))
	ws := mustNewSession(t, writable, "images/readonly")
	if err := ws.SetActualState(ctx, &Status[testDetails]{
		ObservedGeneration: "sha1",
		Details:            testDetails{Message: "written"},
	}); err != nil {
		t.Fatalf("SetActualState() error = %v", err)
	}

	// Read-only manager can read.
	ro := NewReadOnly[testDetails]("test-identity", client.Bucket("test-bucket"))
	rs := mustNewSession(t, ro, "images/readonly")

	got, err := rs.ObservedState(ctx)
	if err != nil {
		t.Fatalf("ObservedState() error = %v", err)
	}
	want := &Status[testDetails]{
		ObservedGeneration: "sha1",
		Details:            testDetails{Message: "written"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ObservedState() mismatch (-want +got):\n%s", diff)
	}

	// Read-only manager cannot write.
	err = rs.SetActualState(ctx, &Status[testDetails]{
		ObservedGeneration: "sha2",
		Details:            testDetails{Message: "should-fail"},
	})
	if err == nil {
		t.Fatal("SetActualState() on read-only manager should fail")
	}
}

func TestSetActualState_NilStatus(t *testing.T) {
	client := newTestClient(t)

	m := New[testDetails]("test-identity", client.Bucket("test-bucket"))
	session := mustNewSession(t, m, "images/nil")

	err := session.SetActualState(t.Context(), nil)
	if err == nil {
		t.Fatal("SetActualState(nil) should fail")
	}
}

func TestInvalidKeys(t *testing.T) {
	client := newTestClient(t)
	m := New[testDetails]("test-identity", client.Bucket("test-bucket"))

	for _, key := range []string{"/absolute", "../escape", "foo/../../escape", "foo/../bar"} {
		_, err := m.NewSession(key)
		if err == nil {
			t.Errorf("NewSession(%q) should fail", key)
		}
	}
}

func TestDifferentKeys_Independent(t *testing.T) {
	client := newTestClient(t)
	ctx := t.Context()

	m := New[testDetails]("test-identity", client.Bucket("test-bucket"))

	s1 := mustNewSession(t, m, "images/alpha")
	s2 := mustNewSession(t, m, "images/beta")

	if err := s1.SetActualState(ctx, &Status[testDetails]{
		ObservedGeneration: "sha-alpha",
		Details:            testDetails{Message: "alpha"},
	}); err != nil {
		t.Fatalf("SetActualState(alpha) error = %v", err)
	}

	if err := s2.SetActualState(ctx, &Status[testDetails]{
		ObservedGeneration: "sha-beta",
		Details:            testDetails{Message: "beta"},
	}); err != nil {
		t.Fatalf("SetActualState(beta) error = %v", err)
	}

	got1, err := s1.ObservedState(ctx)
	if err != nil {
		t.Fatalf("ObservedState(alpha) error = %v", err)
	}
	want1 := &Status[testDetails]{ObservedGeneration: "sha-alpha", Details: testDetails{Message: "alpha"}}
	if diff := cmp.Diff(want1, got1); diff != "" {
		t.Errorf("ObservedState(alpha) mismatch (-want +got):\n%s", diff)
	}

	got2, err := s2.ObservedState(ctx)
	if err != nil {
		t.Fatalf("ObservedState(beta) error = %v", err)
	}
	want2 := &Status[testDetails]{ObservedGeneration: "sha-beta", Details: testDetails{Message: "beta"}}
	if diff := cmp.Diff(want2, got2); diff != "" {
		t.Errorf("ObservedState(beta) mismatch (-want +got):\n%s", diff)
	}
}

func TestDifferentIdentities_Independent(t *testing.T) {
	client := newTestClient(t)
	ctx := t.Context()

	m1 := New[testDetails]("identity-a", client.Bucket("test-bucket"))
	m2 := New[testDetails]("identity-b", client.Bucket("test-bucket"))

	s1 := mustNewSession(t, m1, "images/foo")
	s2 := mustNewSession(t, m2, "images/foo")

	if err := s1.SetActualState(ctx, &Status[testDetails]{
		ObservedGeneration: "sha1",
		Details:            testDetails{Message: "from-a"},
	}); err != nil {
		t.Fatalf("SetActualState(a) error = %v", err)
	}

	// identity-b should not see identity-a's status.
	got, err := s2.ObservedState(ctx)
	if err != nil {
		t.Fatalf("ObservedState(b) error = %v", err)
	}
	if got != nil {
		t.Fatalf("ObservedState(b) = %v, want nil (different identity)", got)
	}
}

func mustNewSession(t *testing.T, m *Manager[testDetails], key string) *Session[testDetails] {
	t.Helper()
	s, err := m.NewSession(key)
	if err != nil {
		t.Fatalf("NewSession(%q) error = %v", key, err)
	}
	return s
}

// fakeGCS is a minimal in-process fake for the GCS JSON API.
type fakeGCS struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

func newFakeGCS() *fakeGCS {
	return &fakeGCS{objects: make(map[string][]byte)}
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Upload: POST /upload/storage/v1/b/{bucket}/o?uploadType=...&name={object}
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/upload/storage/v1/b/") {
		f.handleUpload(w, r)
		return
	}

	// Download: GET /storage/v1/b/{bucket}/o/{object}?alt=media
	if r.Method == http.MethodGet && r.URL.Query().Get("alt") == "media" {
		f.handleDownload(w, r)
		return
	}

	// Metadata: GET /storage/v1/b/{bucket}/o/{object} (no alt=media)
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/o/") {
		f.handleMetadata(w, r)
		return
	}

	w.WriteHeader(http.StatusNotImplemented)
	fmt.Fprintf(w, `{"error":{"code":501,"message":"fake GCS: unsupported %s %s"}}`, r.Method, r.URL.Path)
}

func (f *fakeGCS) handleUpload(w http.ResponseWriter, r *http.Request) {
	bucket := extractBucket(r.URL.Path, "/upload/storage/v1/b/")
	object := r.URL.Query().Get("name")
	if bucket == "" || object == "" {
		http.Error(w, "missing bucket or object name", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Parse the multipart body to extract the content (second part).
	ct := r.Header.Get("Content-Type")
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		http.Error(w, fmt.Sprintf("parsing content-type: %v", err), http.StatusBadRequest)
		return
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	// Skip the first part (JSON metadata).
	if _, err := mr.NextPart(); err != nil {
		http.Error(w, fmt.Sprintf("reading metadata part: %v", err), http.StatusBadRequest)
		return
	}
	// Read the second part (actual content).
	part, err := mr.NextPart()
	if err != nil {
		http.Error(w, fmt.Sprintf("reading content part: %v", err), http.StatusBadRequest)
		return
	}
	content, err := io.ReadAll(part)
	if err != nil {
		http.Error(w, fmt.Sprintf("reading content: %v", err), http.StatusInternalServerError)
		return
	}

	key := bucket + "/" + object
	f.mu.Lock()
	f.objects[key] = content
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"name":%q,"bucket":%q,"size":"%d"}`, object, bucket, len(content))
}

func (f *fakeGCS) handleDownload(w http.ResponseWriter, r *http.Request) {
	bucket, object := extractBucketAndObject(r.URL.Path)
	key := bucket + "/" + object

	f.mu.RLock()
	data, ok := f.objects[key]
	f.mu.RUnlock()

	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":{"code":404,"message":"No such object: %s","errors":[{"reason":"notFound"}]}}`, key)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (f *fakeGCS) handleMetadata(w http.ResponseWriter, r *http.Request) {
	bucket, object := extractBucketAndObject(r.URL.Path)
	key := bucket + "/" + object

	f.mu.RLock()
	data, ok := f.objects[key]
	f.mu.RUnlock()

	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":{"code":404,"message":"No such object: %s","errors":[{"reason":"notFound"}]}}`, key)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"name":%q,"bucket":%q,"size":"%d","contentType":"application/json"}`, object, bucket, len(data))
}

// extractBucket extracts the bucket name from a URL path with the given prefix.
func extractBucket(urlPath, prefix string) string {
	rest := strings.TrimPrefix(urlPath, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// extractBucketAndObject extracts bucket and object from paths like
// /storage/v1/b/{bucket}/o/{object}
func extractBucketAndObject(urlPath string) (string, string) {
	const prefix = "/storage/v1/b/"
	rest := strings.TrimPrefix(urlPath, prefix)
	parts := strings.SplitN(rest, "/o/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func newTestClient(t *testing.T) *storage.Client {
	t.Helper()
	fake := newFakeGCS()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	client, err := storage.NewClient(t.Context(),
		option.WithEndpoint(server.URL+"/storage/v1/"),
		option.WithoutAuthentication(),
		storage.WithJSONReads(),
	)
	if err != nil {
		t.Fatalf("creating storage client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return client
}
