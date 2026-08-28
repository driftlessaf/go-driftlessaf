/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/transient"
	"github.com/google/go-github/v88/github"
)

const (
	testContentsPath = "/api/v3/repos/tests/repo/contents/packages/foo.yaml"
	testBlobPath     = "/api/v3/repos/tests/repo/git/blobs/bigsha"
)

// newFetchTestClient starts a server speaking the GitHub REST API and returns a
// client pointed at it. WithEnterpriseURLs appends api/v3/ to any base URL whose
// host is not api.*, so the prefix is passed explicitly here and handlers are
// mounted under it.
func newFetchTestClient(t *testing.T, handler http.Handler) *github.Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	gh, err := github.NewClient(github.WithEnterpriseURLs(srv.URL+"/api/v3/", srv.URL+"/api/v3/"))
	if err != nil {
		t.Fatalf("github.NewClient: %v", err)
	}
	return gh
}

func fetchTestResource(path, ref string) *githubreconciler.Resource {
	return &githubreconciler.Resource{
		Owner: "tests",
		Repo:  "repo",
		Type:  githubreconciler.ResourceTypePath,
		Ref:   ref,
		Path:  path,
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

func fileResponse(content string) map[string]any {
	return map[string]any{
		"type":     "file",
		"encoding": "base64",
		"name":     "foo.yaml",
		"path":     "packages/foo.yaml",
		"sha":      "deadbeef",
		"size":     len(content),
		"content":  base64.StdEncoding.EncodeToString([]byte(content)),
	}
}

func TestFetchFile(t *testing.T) {
	const want = "contents: yes\n"

	var gotRef string
	mux := http.NewServeMux()
	mux.HandleFunc(testContentsPath, func(w http.ResponseWriter, r *http.Request) {
		gotRef = r.URL.Query().Get("ref")
		writeJSON(t, w, fileResponse(want))
	})

	gh := newFetchTestClient(t, mux)
	file, err := FetchFile(t.Context(), gh, fetchTestResource("packages/foo.yaml", "main"))
	if err != nil {
		t.Fatalf("FetchFile(packages/foo.yaml@main) returned error %v, expected success", err)
	}

	if got := string(file.Content); got != want {
		t.Errorf("FetchFile(packages/foo.yaml@main).Content: got = %q, wanted = %q", got, want)
	}
	if file.SHA != "deadbeef" {
		t.Errorf("FetchFile(packages/foo.yaml@main).SHA: got = %q, wanted = %q", file.SHA, "deadbeef")
	}
	if file.Path != "packages/foo.yaml" {
		t.Errorf("FetchFile(packages/foo.yaml@main).Path: got = %q, wanted = %q", file.Path, "packages/foo.yaml")
	}
	if gotRef != "main" {
		t.Errorf("FetchFile(packages/foo.yaml@main) requested ref: got = %q, wanted = %q", gotRef, "main")
	}
}

func TestFetchFileRef_OverridesResourceRef(t *testing.T) {
	var gotRef string
	mux := http.NewServeMux()
	mux.HandleFunc(testContentsPath, func(w http.ResponseWriter, r *http.Request) {
		gotRef = r.URL.Query().Get("ref")
		writeJSON(t, w, fileResponse("x"))
	})

	gh := newFetchTestClient(t, mux)
	res := fetchTestResource("packages/foo.yaml", "main")
	if _, err := FetchFileRef(t.Context(), gh, res, "v1.2.3"); err != nil {
		t.Fatalf("FetchFileRef(packages/foo.yaml, v1.2.3) returned error %v, expected success", err)
	}

	if gotRef != "v1.2.3" {
		t.Errorf("FetchFileRef(packages/foo.yaml, v1.2.3) requested ref: got = %q, wanted = %q", gotRef, "v1.2.3")
	}
}

func TestFetchFile_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(testContentsPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(t, w, map[string]any{"message": "Not Found"})
	})

	gh := newFetchTestClient(t, mux)
	_, err := FetchFile(t.Context(), gh, fetchTestResource("packages/foo.yaml", "main"))
	if !errors.Is(err, ErrFileNotFound) {
		t.Errorf("FetchFile(packages/foo.yaml@main) on a 404: got = %v, wanted = an error wrapping ErrFileNotFound", err)
	}
}

func TestFetchFile_Directory(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/tests/repo/contents/packages", func(w http.ResponseWriter, _ *http.Request) {
		// A directory comes back as an array, which leaves go-github's file
		// return value nil.
		writeJSON(t, w, []map[string]any{
			{"type": "file", "name": "foo.yaml", "path": "packages/foo.yaml", "sha": "deadbeef"},
		})
	})

	gh := newFetchTestClient(t, mux)
	_, err := FetchFile(t.Context(), gh, fetchTestResource("packages", "main"))
	if err == nil {
		t.Fatal("FetchFile(packages@main) on a directory: got = nil, wanted = an error")
	}
	if errors.Is(err, ErrFileNotFound) {
		t.Errorf("FetchFile(packages@main) on a directory: got = %v, wanted = an error that does not wrap ErrFileNotFound", err)
	}
}

func TestFetchFile_NotARegularFile(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(testContentsPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"type":   "symlink",
			"name":   "foo.yaml",
			"path":   "packages/foo.yaml",
			"sha":    "deadbeef",
			"target": "../elsewhere/foo.yaml",
		})
	})

	gh := newFetchTestClient(t, mux)
	_, err := FetchFile(t.Context(), gh, fetchTestResource("packages/foo.yaml", "main"))
	if err == nil {
		t.Fatal("FetchFile(packages/foo.yaml@main) on a symlink: got = nil, wanted = an error")
	}
}

func TestFetchFile_OverInlineLimitFallsBackToBlob(t *testing.T) {
	const want = "large file contents that GitHub declined to inline\n"

	var blobHits int
	var gotAccept string
	mux := http.NewServeMux()
	mux.HandleFunc(testContentsPath, func(w http.ResponseWriter, _ *http.Request) {
		// Over 1MB GitHub omits the content and sets encoding "none", but still
		// reports the blob SHA.
		writeJSON(t, w, map[string]any{
			"type":     "file",
			"encoding": "none",
			"name":     "foo.yaml",
			"path":     "packages/foo.yaml",
			"sha":      "bigsha",
			"size":     2 << 20,
			"content":  "",
		})
	})
	mux.HandleFunc(testBlobPath, func(w http.ResponseWriter, r *http.Request) {
		blobHits++
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/vnd.github.v3.raw")
		if _, err := w.Write([]byte(want)); err != nil {
			t.Errorf("writing blob response: %v", err)
		}
	})

	gh := newFetchTestClient(t, mux)
	file, err := FetchFile(t.Context(), gh, fetchTestResource("packages/foo.yaml", "main"))
	if err != nil {
		t.Fatalf("FetchFile(packages/foo.yaml@main) on an over-1MB file returned error %v, expected success", err)
	}

	if got := string(file.Content); got != want {
		t.Errorf("FetchFile(packages/foo.yaml@main).Content: got = %q, wanted = %q", got, want)
	}
	if file.SHA != "bigsha" {
		t.Errorf("FetchFile(packages/foo.yaml@main).SHA: got = %q, wanted = %q", file.SHA, "bigsha")
	}
	if blobHits != 1 {
		t.Errorf("FetchFile(packages/foo.yaml@main) blobs API requests: got = %d, wanted = 1", blobHits)
	}
	// The raw media type is what keeps the blob out of base64; without it GitHub
	// sends the encoded JSON envelope and Content would be that envelope.
	if want := "application/vnd.github.v3.raw"; gotAccept != want {
		t.Errorf("FetchFile(packages/foo.yaml@main) blobs API Accept header: got = %q, wanted = %q", gotAccept, want)
	}
}

func TestFetchFile_RateLimitNotRetried(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc(testContentsPath, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		writeJSON(t, w, map[string]any{"message": "API rate limit exceeded"})
	})

	gh := newFetchTestClient(t, mux)
	_, err := FetchFile(t.Context(), gh, fetchTestResource("packages/foo.yaml", "main"))

	// The error must reach Reconcile as a *github.RateLimitError so it requeues
	// at the reset time rather than retrying against an exhausted quota.
	if _, ok := errors.AsType[*github.RateLimitError](err); !ok {
		t.Errorf("FetchFile(packages/foo.yaml@main) when rate limited: got = %v, wanted = an error wrapping *github.RateLimitError", err)
	}
	if hits != 1 {
		t.Errorf("FetchFile(packages/foo.yaml@main) when rate limited, requests: got = %d, wanted = 1", hits)
	}
}

// TestFetchFile_RetriesServerErrors sleeps between attempts (transient.Retry
// waits 1s plus up to 2s of jitter), so it runs for a few seconds.
func TestFetchFile_RetriesServerErrors(t *testing.T) {
	const want = "recovered\n"

	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc(testContentsPath, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(t, w, map[string]any{"message": "Server Error"})
			return
		}
		writeJSON(t, w, fileResponse(want))
	})

	gh := newFetchTestClient(t, mux)
	file, err := FetchFile(t.Context(), gh, fetchTestResource("packages/foo.yaml", "main"))
	if err != nil {
		t.Fatalf("FetchFile(packages/foo.yaml@main) after two 500s returned error %v, expected success", err)
	}

	if got := string(file.Content); got != want {
		t.Errorf("FetchFile(packages/foo.yaml@main).Content: got = %q, wanted = %q", got, want)
	}
	if hits != 3 {
		t.Errorf("FetchFile(packages/foo.yaml@main) after two 500s, requests: got = %d, wanted = 3", hits)
	}
}

func TestFetchFile_Validation(t *testing.T) {
	gh := newFetchTestClient(t, http.NotFoundHandler())

	for _, tc := range []struct {
		name string
		res  *githubreconciler.Resource
	}{{
		name: "nil resource",
		res:  nil,
	}, {
		name: "issue resource",
		res: &githubreconciler.Resource{
			Owner: "tests", Repo: "repo", Number: 1,
			Type: githubreconciler.ResourceTypeIssue,
		},
	}, {
		name: "empty owner",
		res: &githubreconciler.Resource{
			Type: githubreconciler.ResourceTypePath, Repo: "repo",
			Ref: "main", Path: "packages/foo.yaml",
		},
	}, {
		name: "empty repo",
		res: &githubreconciler.Resource{
			Type: githubreconciler.ResourceTypePath, Owner: "tests",
			Ref: "main", Path: "packages/foo.yaml",
		},
	}, {
		name: "empty path",
		res:  fetchTestResource("", "main"),
	}, {
		name: "empty ref",
		res:  fetchTestResource("packages/foo.yaml", ""),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FetchFile(t.Context(), gh, tc.res); err == nil {
				t.Errorf("FetchFile(%v): got = nil, wanted = a validation error", tc.res)
			}
		})
	}
}

func TestRetryableFetchErr(t *testing.T) {
	ghErr := func(code int) error {
		return &github.ErrorResponse{Response: &http.Response{StatusCode: code}}
	}
	rateLimitErr := func(code int) error {
		return &github.RateLimitError{Response: &http.Response{StatusCode: code}}
	}
	abuseErr := func(code int) error {
		return &github.AbuseRateLimitError{Response: &http.Response{StatusCode: code}}
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{{
		name: "server error",
		err:  ghErr(http.StatusInternalServerError),
		want: true,
	}, {
		name: "bad gateway",
		err:  ghErr(http.StatusBadGateway),
		want: true,
	}, {
		name: "wrapped server error",
		err:  fmt.Errorf("getting contents: %w", ghErr(http.StatusServiceUnavailable)),
		want: true,
	}, {
		name: "transport failure",
		err:  &url.Error{Op: "Get", URL: "https://api.github.com", Err: errors.New("connection reset by peer")},
		want: true,
	}, {
		name: "primary rate limit",
		err:  rateLimitErr(http.StatusForbidden),
		want: false,
	}, {
		name: "secondary rate limit",
		err:  abuseErr(http.StatusForbidden),
		want: false,
	}, {
		name: "not found",
		err:  fmt.Errorf("packages/foo.yaml at main: %w", ErrFileNotFound),
		want: false,
	}, {
		name: "unauthorized",
		err:  ghErr(http.StatusUnauthorized),
		want: false,
	}, {
		name: "unprocessable entity",
		err:  ghErr(http.StatusUnprocessableEntity),
		want: false,
	}, {
		name: "unclassified error",
		err:  errors.New("something else"),
		want: false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableFetchErr(tc.err); got != tc.want {
				t.Errorf("retryableFetchErr(%v): got = %t, wanted = %t", tc.err, got, tc.want)
			}
		})
	}
}

// TestFetchFile_ExhaustedRetriesMarkedTransient pins the requeue contract for
// persistent server errors: Reconcile keys off transient.Is. It sleeps between
// attempts, so it runs for a few seconds.
func TestFetchFile_ExhaustedRetriesMarkedTransient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(testContentsPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(t, w, map[string]any{"message": "Server Error"})
	})

	gh := newFetchTestClient(t, mux)
	_, err := FetchFile(t.Context(), gh, fetchTestResource("packages/foo.yaml", "main"))
	if err == nil {
		t.Fatal("FetchFile(packages/foo.yaml@main) against a failing server: got = nil, wanted = an error")
	}
	if !transient.Is(err) {
		t.Errorf("transient.Is(FetchFile(packages/foo.yaml@main) error): got = false, wanted = true for %v", err)
	}
}
