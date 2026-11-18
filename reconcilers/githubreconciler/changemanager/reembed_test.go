/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/go-github/v88/github"
)

// newReembedServer serves GET /pulls/7 with the given labels and records PATCHes.
func newReembedServer(t *testing.T, labels ...string) (*github.Client, *[]string) {
	t.Helper()
	var patches []string
	prLabels := make([]*github.Label, 0, len(labels))
	for _, l := range labels {
		prLabels = append(prLabels, &github.Label{Name: new(l)})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, &github.PullRequest{Number: new(7), Body: new("PR body text"), Labels: prLabels})
	})
	mux.HandleFunc("PATCH /api/v3/repos/test-owner/test-repo/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		patches = append(patches, r.URL.Path)
		writeJSON(t, w, &github.PullRequest{Number: new(7)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	return client, &patches
}

func newReembedSession(t *testing.T, client *github.Client) *Session[testData] {
	t.Helper()
	s := newMarkerSession(client, 7)
	s.manager = &CM[testData]{identity: "test-bot", templateExecutor: mustTemplateExecutor(t)}
	return s
}

func TestReembedDataRefusesSkipLabel(t *testing.T) {
	client, patches := newReembedServer(t, "skip:test-bot")
	s := newReembedSession(t, client)

	if err := s.ReembedData(t.Context(), &testData{}); err == nil {
		t.Error("ReembedData: got error = nil, want skip-label error")
	}
	if len(*patches) != 0 {
		t.Errorf("PATCH calls: got = %v, want none", *patches)
	}
}

func TestReembedDataWritesWithoutSkipLabel(t *testing.T) {
	client, patches := newReembedServer(t, "automated pr")
	s := newReembedSession(t, client)

	if err := s.ReembedData(t.Context(), &testData{}); err != nil {
		t.Fatalf("ReembedData: got error = %v, want = nil", err)
	}
	if len(*patches) != 1 {
		t.Errorf("PATCH calls: got = %v, want 1", *patches)
	}
}

func TestClearReadyForReviewNoOpWithoutLabel(t *testing.T) {
	// The server has no label endpoints, so any removal attempt would fail.
	client, _ := newReembedServer(t)
	s := newReembedSession(t, client)
	s.prLabels = []string{"automated pr"}

	if _, err := s.ClearReadyForReview(t.Context()); err != nil {
		t.Fatalf("ClearReadyForReview: got error = %v, want = nil", err)
	}
	if !slices.Equal(s.prLabels, []string{"automated pr"}) {
		t.Errorf("labels: got = %v, want unchanged", s.prLabels)
	}
}
