/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/transient"
	"github.com/google/go-github/v88/github"
)

// ErrFileNotFound is returned by FetchFile when the path does not exist at the
// requested ref.
var ErrFileNotFound = errors.New("file not found")

// File is a single file read from a repository.
type File struct {
	// Path is the path within the repository, as GitHub reported it.
	Path string
	// SHA is the git blob SHA of Content.
	SHA     string
	Content []byte
}

// FetchFile reads the file named by res over the GitHub REST API, without
// cloning. The resource must be of type githubreconciler.ResourceTypePath; the
// file is read at res.Ref. Callers pass the client their ReconcilerFunc already
// received, so the read shares that client's credentials and instrumentation.
// Returns an error wrapping ErrFileNotFound when the path does not exist at that
// ref.
func FetchFile(ctx context.Context, gh *github.Client, res *githubreconciler.Resource) (*File, error) {
	if res == nil {
		return nil, errors.New("resource cannot be nil")
	}
	return fetchFile(ctx, gh, res, res.Ref)
}

// FetchFileRef reads res.Path at the supplied ref rather than res.Ref. The ref
// can be a branch, tag, or commit SHA.
func FetchFileRef(ctx context.Context, gh *github.Client, res *githubreconciler.Resource, ref string) (*File, error) {
	if res == nil {
		return nil, errors.New("resource cannot be nil")
	}
	return fetchFile(ctx, gh, res, ref)
}

func fetchFile(ctx context.Context, gh *github.Client, res *githubreconciler.Resource, ref string) (*File, error) {
	switch {
	case gh == nil:
		return nil, errors.New("github client cannot be nil")
	case res.Type != githubreconciler.ResourceTypePath:
		return nil, fmt.Errorf("unsupported resource type %q: FetchFile requires %q", res.Type, githubreconciler.ResourceTypePath)
	case res.Owner == "":
		return nil, errors.New("resource owner cannot be empty")
	case res.Repo == "":
		return nil, errors.New("resource repo cannot be empty")
	case res.Path == "":
		return nil, errors.New("resource path cannot be empty")
	case ref == "":
		return nil, errors.New("ref cannot be empty")
	}

	var file *File
	if err := transient.Retry(ctx, fmt.Sprintf("fetching %s/%s@%s:%s", res.Owner, res.Repo, ref, res.Path),
		retryableFetchErr, func(ctx context.Context) error {
			var err error
			file, err = fetchFileOnce(ctx, gh, res, ref)
			return err
		}); err != nil {
		return nil, err
	}
	return file, nil
}

func fetchFileOnce(ctx context.Context, gh *github.Client, res *githubreconciler.Resource, ref string) (*File, error) {
	fc, _, _, err := gh.Repositories.GetContents(ctx, res.Owner, res.Repo, res.Path,
		&github.RepositoryContentGetOptions{Ref: ref})
	switch {
	case isGitHubNotFound(err):
		return nil, fmt.Errorf("%s at %s: %w", res.Path, ref, ErrFileNotFound)
	case err != nil:
		return nil, fmt.Errorf("getting contents of %s at %s: %w", res.Path, ref, err)
	case fc == nil:
		// GetContents reports a directory through its other return value,
		// leaving fc nil. GetContent on that nil pointer would panic.
		return nil, fmt.Errorf("%s at %s is a directory, not a file", res.Path, ref)
	case fc.GetType() != "file":
		return nil, fmt.Errorf("%s at %s is a %q, not a file", res.Path, ref, fc.GetType())
	}

	// GitHub declines to inline blobs over 1MB: encoding "none", no content, but
	// still a blob SHA. The blobs API serves those, up to its own 100MB ceiling.
	// This has to be checked before GetContent, which reports the refusal as an
	// opaque error.
	if fc.GetEncoding() == "none" {
		content, err := fetchBlob(ctx, gh, res.Owner, res.Repo, fc.GetSHA())
		if err != nil {
			return nil, err
		}
		return &File{Path: fc.GetPath(), SHA: fc.GetSHA(), Content: content}, nil
	}

	content, err := fc.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decoding %s at %s: %w", res.Path, ref, err)
	}
	return &File{Path: fc.GetPath(), SHA: fc.GetSHA(), Content: []byte(content)}, nil
}

// fetchBlob reads a blob through the raw media type. GetBlob would transfer the
// same bytes base64-encoded and hold both the encoded string and the decoded
// copy live at once, which near the blobs API's 100MB ceiling costs both
// bandwidth and peak memory for no benefit.
func fetchBlob(ctx context.Context, gh *github.Client, owner, repo, sha string) ([]byte, error) {
	content, _, err := gh.Git.GetBlobRaw(ctx, owner, repo, sha)
	if err != nil {
		return nil, fmt.Errorf("getting blob %s: %w", sha, err)
	}
	return content, nil
}

// isGitHubNotFound reports whether err is a GitHub 404.
func isGitHubNotFound(err error) bool {
	errResp, ok := errors.AsType[*github.ErrorResponse](err)
	return ok && errResp.Response != nil && errResp.Response.StatusCode == http.StatusNotFound
}

// retryableFetchErr reports whether a fetch failure is worth retrying in-process.
// Rate limits are deliberately excluded: they carry a reset time that
// githubreconciler.Reconciler.Reconcile turns into a requeue, and retrying them
// here would only spend the remaining quota.
func retryableFetchErr(err error) bool {
	if _, ok := errors.AsType[*github.RateLimitError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*github.AbuseRateLimitError](err); ok {
		return false
	}
	if errors.Is(err, ErrFileNotFound) {
		return false
	}

	if errResp, ok := errors.AsType[*github.ErrorResponse](err); ok && errResp.Response != nil {
		return errResp.Response.StatusCode >= http.StatusInternalServerError
	}

	// Transport-level failures: connection resets, DNS blips, timeouts.
	if _, ok := errors.AsType[*url.Error](err); ok {
		return true
	}
	netErr, ok := errors.AsType[net.Error](err)
	return ok && netErr.Timeout()
}
