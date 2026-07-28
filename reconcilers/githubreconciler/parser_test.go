/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"reflect"
	"testing"
)

func TestParseURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    *Resource
		wantErr bool
	}{{
		name: "valid issue URL",
		url:  "https://github.com/owner/repo/issues/123",
		want: &Resource{
			Owner:  "owner",
			Repo:   "repo",
			Type:   ResourceTypeIssue,
			Number: 123,
			URL:    "https://github.com/owner/repo/issues/123",
		},
	}, {
		name: "valid pull request URL",
		url:  "https://github.com/owner/repo/pull/456",
		want: &Resource{
			Owner:  "owner",
			Repo:   "repo",
			Type:   ResourceTypePullRequest,
			Number: 456,
			URL:    "https://github.com/owner/repo/pull/456",
		},
	}, {
		name:    "invalid URL - www.github.com",
		url:     "https://www.github.com/owner/repo/issues/789",
		wantErr: true,
	}, {
		name: "owner with hyphen",
		url:  "https://github.com/my-owner/repo/issues/1",
		want: &Resource{
			Owner:  "my-owner",
			Repo:   "repo",
			Type:   ResourceTypeIssue,
			Number: 1,
			URL:    "https://github.com/my-owner/repo/issues/1",
		},
	}, {
		name: "repo with dots and hyphens",
		url:  "https://github.com/owner/my.complex-repo.name/pull/42",
		want: &Resource{
			Owner:  "owner",
			Repo:   "my.complex-repo.name",
			Type:   ResourceTypePullRequest,
			Number: 42,
			URL:    "https://github.com/owner/my.complex-repo.name/pull/42",
		},
	}, {
		name: "valid blob path URL",
		url:  "https://github.com/owner/repo/blob/main/README.md",
		want: &Resource{
			Owner: "owner",
			Repo:  "repo",
			Type:  ResourceTypePath,
			Ref:   "main",
			Path:  "README.md",
			URL:   "https://github.com/owner/repo/blob/main/README.md",
		},
	}, {
		name: "valid tree path URL",
		url:  "https://github.com/owner/repo/tree/main/docs",
		want: &Resource{
			Owner: "owner",
			Repo:  "repo",
			Type:  ResourceTypePath,
			Ref:   "main",
			Path:  "docs",
			URL:   "https://github.com/owner/repo/tree/main/docs",
		},
	}, {
		name: "path URL with nested path",
		url:  "https://github.com/owner/repo/blob/main/pkg/foo/bar.go",
		want: &Resource{
			Owner: "owner",
			Repo:  "repo",
			Type:  ResourceTypePath,
			Ref:   "main",
			Path:  "pkg/foo/bar.go",
			URL:   "https://github.com/owner/repo/blob/main/pkg/foo/bar.go",
		},
	}, {
		name: "path URL with tag ref",
		url:  "https://github.com/owner/repo/blob/v1.2.3/cmd/main.go",
		want: &Resource{
			Owner: "owner",
			Repo:  "repo",
			Type:  ResourceTypePath,
			Ref:   "v1.2.3",
			Path:  "cmd/main.go",
			URL:   "https://github.com/owner/repo/blob/v1.2.3/cmd/main.go",
		},
	}, {
		name: "path URL with SHA ref",
		url:  "https://github.com/owner/repo/blob/abc123def456/docs/guide.md",
		want: &Resource{
			Owner: "owner",
			Repo:  "repo",
			Type:  ResourceTypePath,
			Ref:   "abc123def456",
			Path:  "docs/guide.md",
			URL:   "https://github.com/owner/repo/blob/abc123def456/docs/guide.md",
		},
	}, {
		name: "path URL with deeply nested path",
		url:  "https://github.com/owner/repo/tree/main/a/b/c/d/e/f",
		want: &Resource{
			Owner: "owner",
			Repo:  "repo",
			Type:  ResourceTypePath,
			Ref:   "main",
			Path:  "a/b/c/d/e/f",
			URL:   "https://github.com/owner/repo/tree/main/a/b/c/d/e/f",
		},
	}, {
		name:    "invalid URL - wrong host",
		url:     "https://gitlab.com/owner/repo/issues/123",
		wantErr: true,
	}, {
		name:    "invalid URL - no issue/PR type",
		url:     "https://github.com/owner/repo/123",
		wantErr: true,
	}, {
		name:    "invalid URL - no number",
		url:     "https://github.com/owner/repo/issues",
		wantErr: true,
	}, {
		name:    "invalid URL - non-numeric number",
		url:     "https://github.com/owner/repo/issues/abc",
		wantErr: true,
	}, {
		name:    "invalid URL - missing owner",
		url:     "https://github.com/repo/issues/123",
		wantErr: true,
	}, {
		name:    "invalid URL - empty string",
		url:     "",
		wantErr: true,
	}, {
		name:    "invalid URL - not a URL",
		url:     "not-a-url",
		wantErr: true,
	}, {
		name:    "invalid URL - wrong resource type",
		url:     "https://github.com/owner/repo/commits/123",
		wantErr: true,
	}, {
		name:    "invalid URL - too many path segments",
		url:     "https://github.com/owner/repo/issues/123/comments",
		wantErr: true,
	}, {
		name:    "invalid URL - too few path segments",
		url:     "https://github.com/owner",
		wantErr: true,
	}, {
		name: "http URL still works",
		url:  "http://github.com/owner/repo/issues/123",
		want: &Resource{
			Owner:  "owner",
			Repo:   "repo",
			Type:   ResourceTypeIssue,
			Number: 123,
			URL:    "http://github.com/owner/repo/issues/123",
		},
	}, {
		name: "scheme-relative tree key",
		url:  "github.com/owner/repo/tree/HEAD/.github/workflows",
		want: &Resource{
			Owner: "owner",
			Repo:  "repo",
			Type:  ResourceTypePath,
			Ref:   "HEAD",
			Path:  ".github/workflows",
			// URL preserves the original scheme-relative key verbatim so it
			// round-trips back to the workqueue.
			URL: "github.com/owner/repo/tree/HEAD/.github/workflows",
		},
	}, {
		name: "scheme-relative issue key",
		url:  "github.com/owner/repo/issues/123",
		want: &Resource{
			Owner:  "owner",
			Repo:   "repo",
			Type:   ResourceTypeIssue,
			Number: 123,
			URL:    "github.com/owner/repo/issues/123",
		},
	}, {
		name:    "scheme-relative wrong host",
		url:     "gitlab.com/owner/repo/issues/123",
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseURL() error: got = %v, wanted = %v", err != nil, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseURL(): got = %v, wanted = %v", got, tt.want)
			}
		})
	}
}

// TestParseURL_SchemeRelativeBackwardCompatible proves the migration of migrate
// workqueue keys to drop the "https://" scheme is backward compatible at the
// consume boundary: the legacy scheme-ful key and the new scheme-relative key
// resolve to the same resource (owner, repo, type, ref, path), differing only in
// the verbatim-preserved URL field. In-flight legacy keys therefore route
// identically while they drain.
func TestParseURL_SchemeRelativeBackwardCompatible(t *testing.T) {
	const (
		legacy = "https://github.com/owner/repo/tree/HEAD/.github/workflows"
		modern = "github.com/owner/repo/tree/HEAD/.github/workflows"
	)

	legacyRes, err := ParseURL(legacy)
	if err != nil {
		t.Fatalf("ParseURL(legacy) error: %v", err)
	}
	modernRes, err := ParseURL(modern)
	if err != nil {
		t.Fatalf("ParseURL(modern) error: %v", err)
	}

	// URL is preserved verbatim so each key round-trips to the workqueue; assert
	// that, then clear it before comparing the resolved resource identity.
	if legacyRes.URL != legacy {
		t.Errorf("legacy URL not preserved: got %q, want %q", legacyRes.URL, legacy)
	}
	if modernRes.URL != modern {
		t.Errorf("modern URL not preserved: got %q, want %q", modernRes.URL, modern)
	}
	legacyRes.URL, modernRes.URL = "", ""
	if !reflect.DeepEqual(legacyRes, modernRes) {
		t.Errorf("legacy and modern keys resolved to different resources:\n legacy = %+v\n modern = %+v", legacyRes, modernRes)
	}
}

func TestResource_String(t *testing.T) {
	tests := []struct {
		name     string
		resource *Resource
		want     string
	}{{
		name: "issue",
		resource: &Resource{
			Owner:  "owner",
			Repo:   "repo",
			Type:   ResourceTypeIssue,
			Number: 123,
		},
		want: "owner/repo#123",
	}, {
		name: "pull request",
		resource: &Resource{
			Owner:  "owner",
			Repo:   "repo",
			Type:   ResourceTypePullRequest,
			Number: 456,
		},
		want: "owner/repo#456",
	}, {
		name: "path with simple file",
		resource: &Resource{
			Owner: "owner",
			Repo:  "repo",
			Type:  ResourceTypePath,
			Ref:   "main",
			Path:  "README.md",
		},
		want: "owner/repo@main:README.md",
	}, {
		name: "path with nested file",
		resource: &Resource{
			Owner: "owner",
			Repo:  "repo",
			Type:  ResourceTypePath,
			Ref:   "main",
			Path:  "pkg/foo/bar.go",
		},
		want: "owner/repo@main:pkg/foo/bar.go",
	}, {
		name: "path with tag ref",
		resource: &Resource{
			Owner: "owner",
			Repo:  "repo",
			Type:  ResourceTypePath,
			Ref:   "v1.2.3",
			Path:  "cmd/main.go",
		},
		want: "owner/repo@v1.2.3:cmd/main.go",
	}, {
		name: "path with SHA ref",
		resource: &Resource{
			Owner: "owner",
			Repo:  "repo",
			Type:  ResourceTypePath,
			Ref:   "abc123def456",
			Path:  "docs/guide.md",
		},
		want: "owner/repo@abc123def456:docs/guide.md",
	}, {
		name: "complex names",
		resource: &Resource{
			Owner:  "my-org",
			Repo:   "my.complex-repo",
			Type:   ResourceTypeIssue,
			Number: 789,
		},
		want: "my-org/my.complex-repo#789",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.resource.String(); got != tt.want {
				t.Errorf("Resource.String(): got = %v, wanted = %v", got, tt.want)
			}
		})
	}
}

func TestParseURL_ValidatesURL(t *testing.T) {
	// Test that URL field is preserved
	urls := []string{
		"https://github.com/owner/repo/issues/123",
		"https://github.com/owner/repo/pull/456",
		"https://github.com/my-org/my.repo/issues/789",
	}

	for _, url := range urls {
		t.Run(url, func(t *testing.T) {
			resource, err := ParseURL(url)
			if err != nil {
				t.Fatalf("ParseURL() error: got = %v, wanted = nil", err)
			}
			if resource.URL != url {
				t.Errorf("Resource.URL: got = %v, wanted = %v", resource.URL, url)
			}
		})
	}
}
