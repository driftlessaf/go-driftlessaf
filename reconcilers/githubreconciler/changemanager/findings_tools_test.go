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

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-github/v88/github"
)

func TestRerunCICheck(t *testing.T) {
	tests := []struct {
		name       string
		detailsURL string
		identifier string
		status     int
		wantErr    string
	}{{
		name:       "valid actions URL",
		detailsURL: "https://github.com/owner/repo/actions/runs/111/job/222",
		status:     http.StatusCreated,
	}, {
		name:       "actions URL with query params",
		detailsURL: "https://github.com/owner/repo/actions/runs/111/job/222?extra=1",
		status:     http.StatusCreated,
	}, {
		name:       "actions job rerun API failure",
		detailsURL: "https://github.com/owner/repo/actions/runs/111/job/333",
		status:     http.StatusForbidden,
		wantErr:    "rerun job 333",
	}, {
		// Non-Actions details URL (e.g. a reconciler's Cloud Logging link):
		// re-requested by check run ID.
		name:       "non-actions URL re-requests by check run ID",
		detailsURL: "https://console.cloud.google.com/logs/query;query=foo",
		identifier: "83331956684",
		status:     http.StatusCreated,
	}, {
		name:       "empty URL re-requests by check run ID",
		detailsURL: "",
		identifier: "555",
		status:     http.StatusCreated,
	}, {
		name:       "non-actions URL with non-numeric identifier",
		detailsURL: "https://github.com/owner/repo/pull/123",
		identifier: "not-a-number",
		wantErr:    "parse check run ID from identifier",
	}, {
		name:       "re-request API failure",
		detailsURL: "",
		identifier: "777",
		status:     http.StatusForbidden,
		wantErr:    "re-request check run 777",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}

			err = rerunCICheck(context.Background(), client, "owner", "repo", callbacks.Finding{
				DetailsURL: tt.detailsURL,
				Identifier: tt.identifier,
			})
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

	client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	if err := rerunCICheck(context.Background(), client, "myorg", "myrepo", callbacks.Finding{
		DetailsURL: "https://github.com/myorg/myrepo/actions/runs/100/job/999",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := "/api/v3/repos/myorg/myrepo/actions/jobs/999/rerun"
	if gotPath != wantPath {
		t.Errorf("API path: got = %q, wanted = %q", gotPath, wantPath)
	}
}

// TestRerunCICheckRerequestRouting verifies a non-Actions finding routes to the
// Checks re-request endpoint, keyed on the check run ID in the Identifier.
func TestRerunCICheckRerequestRouting(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	if err := rerunCICheck(context.Background(), client, "myorg", "myrepo", callbacks.Finding{
		DetailsURL: "https://console.cloud.google.com/logs/query;query=foo",
		Identifier: "83331956684",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := "/api/v3/repos/myorg/myrepo/check-runs/83331956684/rerequest"
	if gotPath != wantPath {
		t.Errorf("API path: got = %q, wanted = %q", gotPath, wantPath)
	}
}
