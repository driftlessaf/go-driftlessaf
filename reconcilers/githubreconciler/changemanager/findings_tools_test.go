/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v84/github"
)

func TestRerunCICheck(t *testing.T) {
	tests := []struct {
		name       string
		detailsURL string
		status     int
		wantErr    string
	}{{
		name:       "empty URL",
		detailsURL: "",
		wantErr:    "finding has no details URL",
	}, {
		name:       "non-actions URL",
		detailsURL: "https://github.com/owner/repo/pull/123",
		wantErr:    "does not match GitHub Actions URL pattern",
	}, {
		name:       "valid actions URL",
		detailsURL: "https://github.com/owner/repo/actions/runs/111/job/222",
		status:     http.StatusCreated,
	}, {
		name:       "actions URL with query params",
		detailsURL: "https://github.com/owner/repo/actions/runs/111/job/222?extra=1",
		status:     http.StatusCreated,
	}, {
		name:       "API failure",
		detailsURL: "https://github.com/owner/repo/actions/runs/111/job/333",
		status:     http.StatusForbidden,
		wantErr:    "rerun job 333",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			client, err := github.NewClient(nil).WithEnterpriseURLs(srv.URL, srv.URL)
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}

			err = rerunCICheck(context.Background(), client, "owner", "repo", tt.detailsURL)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("error: got = nil, wanted containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error: got = %q, wanted containing %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRerunCICheckJobIDRouting(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	client, err := github.NewClient(nil).WithEnterpriseURLs(srv.URL, srv.URL)
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	if err := rerunCICheck(context.Background(), client, "myorg", "myrepo",
		"https://github.com/myorg/myrepo/actions/runs/100/job/999"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := "/api/v3/repos/myorg/myrepo/actions/jobs/999/rerun"
	if gotPath != wantPath {
		t.Errorf("API path: got = %q, wanted = %q", gotPath, wantPath)
	}
}
