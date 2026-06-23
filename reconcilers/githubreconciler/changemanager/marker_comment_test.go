/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"text/template"

	"github.com/google/go-github/v88/github"
)

const testMarker = "<!--test-bot:no-changes-->"

// markerCommentServer wires an httptest server that mimics the GitHub issue
// comment endpoints used by the marker-comment helpers, recording the calls so
// tests can assert what was created, edited, deleted, or skipped.
type markerCommentServer struct {
	existing []*github.IssueComment

	created []string
	edited  map[int64]string
	deleted []int64
}

func newMarkerCommentServer(t *testing.T, existing ...*github.IssueComment) (*github.Client, *markerCommentServer) {
	t.Helper()
	rec := &markerCommentServer{existing: existing, edited: map[int64]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/issues/{number}/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, rec.existing)
	})
	mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request) {
		var c github.IssueComment
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Fatalf("decoding create body: %v", err)
		}
		rec.created = append(rec.created, c.GetBody())
		writeJSON(t, w, &github.IssueComment{ID: github.Ptr(int64(1)), Body: c.Body})
	})
	mux.HandleFunc("PATCH /api/v3/repos/test-owner/test-repo/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		var c github.IssueComment
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Fatalf("decoding edit body: %v", err)
		}
		id := mustParseID(t, r.PathValue("id"))
		rec.edited[id] = c.GetBody()
		writeJSON(t, w, &c)
	})
	mux.HandleFunc("DELETE /api/v3/repos/test-owner/test-repo/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		rec.deleted = append(rec.deleted, mustParseID(t, r.PathValue("id")))
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	return client, rec
}

func mustParseID(t *testing.T, s string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("parsing comment id %q: %v", s, err)
	}
	return id
}

func newMarkerSession(client *github.Client, prNumber int) *Session[testData] {
	return &Session[testData]{
		client:   client,
		owner:    "test-owner",
		repo:     "test-repo",
		prNumber: prNumber,
	}
}

func TestUpsertMarkerComment(t *testing.T) {
	t.Run("creates when absent", func(t *testing.T) {
		client, rec := newMarkerCommentServer(t)
		s := newMarkerSession(client, 7)

		if err := s.UpsertMarkerComment(t.Context(), testMarker, "blocked on foo"); err != nil {
			t.Fatalf("UpsertMarkerComment: got error = %v, want = nil", err)
		}

		want := testMarker + "\nblocked on foo"
		if len(rec.created) != 1 || rec.created[0] != want {
			t.Errorf("created comments: got = %v, want = [%q]", rec.created, want)
		}
		if len(rec.edited) != 0 {
			t.Errorf("edited comments: got = %v, want none", rec.edited)
		}
	})

	t.Run("skips when identical", func(t *testing.T) {
		existing := &github.IssueComment{ID: github.Ptr(int64(42)), Body: github.Ptr(testMarker + "\nblocked on foo")}
		client, rec := newMarkerCommentServer(t, existing)
		s := newMarkerSession(client, 7)

		if err := s.UpsertMarkerComment(t.Context(), testMarker, "blocked on foo"); err != nil {
			t.Fatalf("UpsertMarkerComment: got error = %v, want = nil", err)
		}

		if len(rec.created) != 0 {
			t.Errorf("created comments: got = %v, want none", rec.created)
		}
		if len(rec.edited) != 0 {
			t.Errorf("edited comments: got = %v, want none (identical body must not be rewritten)", rec.edited)
		}
	})

	t.Run("edits when changed", func(t *testing.T) {
		existing := &github.IssueComment{ID: github.Ptr(int64(42)), Body: github.Ptr(testMarker + "\nblocked on foo")}
		client, rec := newMarkerCommentServer(t, existing)
		s := newMarkerSession(client, 7)

		if err := s.UpsertMarkerComment(t.Context(), testMarker, "blocked on bar"); err != nil {
			t.Fatalf("UpsertMarkerComment: got error = %v, want = nil", err)
		}

		want := testMarker + "\nblocked on bar"
		if got := rec.edited[42]; got != want {
			t.Errorf("edited comment 42: got = %q, want = %q", got, want)
		}
		if len(rec.created) != 0 {
			t.Errorf("created comments: got = %v, want none", rec.created)
		}
	})

	t.Run("no-op without PR", func(t *testing.T) {
		client, rec := newMarkerCommentServer(t)
		s := newMarkerSession(client, 0)

		if err := s.UpsertMarkerComment(t.Context(), testMarker, "blocked on foo"); err != nil {
			t.Fatalf("UpsertMarkerComment: got error = %v, want = nil", err)
		}
		if len(rec.created) != 0 || len(rec.edited) != 0 {
			t.Errorf("API mutated without a PR: created = %v, edited = %v", rec.created, rec.edited)
		}
	})
}

func TestDeleteMarkerComment(t *testing.T) {
	t.Run("deletes when present", func(t *testing.T) {
		existing := &github.IssueComment{ID: github.Ptr(int64(42)), Body: github.Ptr(testMarker + "\nblocked on foo")}
		client, rec := newMarkerCommentServer(t, existing)
		s := newMarkerSession(client, 7)

		if err := s.DeleteMarkerComment(t.Context(), testMarker); err != nil {
			t.Fatalf("DeleteMarkerComment: got error = %v, want = nil", err)
		}
		if len(rec.deleted) != 1 || rec.deleted[0] != 42 {
			t.Errorf("deleted comments: got = %v, want = [42]", rec.deleted)
		}
	})

	t.Run("no-op when absent", func(t *testing.T) {
		client, rec := newMarkerCommentServer(t)
		s := newMarkerSession(client, 7)

		if err := s.DeleteMarkerComment(t.Context(), testMarker); err != nil {
			t.Fatalf("DeleteMarkerComment: got error = %v, want = nil", err)
		}
		if len(rec.deleted) != 0 {
			t.Errorf("deleted comments: got = %v, want none", rec.deleted)
		}
	})

	// A human reply that merely quotes the marker mid-body must not be matched:
	// findMarkerComment uses a prefix match, not a substring search.
	t.Run("ignores marker quoted mid-body", func(t *testing.T) {
		quoted := &github.IssueComment{ID: github.Ptr(int64(99)), Body: github.Ptr("> someone said:\n" + testMarker + "\nold reason")}
		client, rec := newMarkerCommentServer(t, quoted)
		s := newMarkerSession(client, 7)

		if err := s.DeleteMarkerComment(t.Context(), testMarker); err != nil {
			t.Fatalf("DeleteMarkerComment: got error = %v, want = nil", err)
		}
		if len(rec.deleted) != 0 {
			t.Errorf("deleted comments: got = %v, want none (marker is quoted, not a prefix)", rec.deleted)
		}
	})
}

// TestMarkerCommentForbiddenIsGraceful verifies that a 403 (missing
// issues:write permission) degrades to a no-op error-free, so a missing
// permission never fails the reconcile.
func TestMarkerCommentForbiddenIsGraceful(t *testing.T) {
	forbid := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	}

	t.Run("upsert when listing is forbidden", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/issues/{number}/comments", forbid)
		s := newMarkerSession(forbiddenServer(t, mux), 7)
		if err := s.UpsertMarkerComment(t.Context(), testMarker, "blocked on foo"); err != nil {
			t.Errorf("UpsertMarkerComment: got error = %v, want = nil (403 must be tolerated)", err)
		}
	})

	t.Run("upsert when creating is forbidden", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/issues/{number}/comments", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, []*github.IssueComment{})
		})
		mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/issues/{number}/comments", forbid)
		s := newMarkerSession(forbiddenServer(t, mux), 7)
		if err := s.UpsertMarkerComment(t.Context(), testMarker, "blocked on foo"); err != nil {
			t.Errorf("UpsertMarkerComment: got error = %v, want = nil (403 must be tolerated)", err)
		}
	})

	t.Run("delete when deleting is forbidden", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/issues/{number}/comments", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, []*github.IssueComment{{ID: github.Ptr(int64(42)), Body: github.Ptr(testMarker + "\nx")}})
		})
		mux.HandleFunc("DELETE /api/v3/repos/test-owner/test-repo/issues/comments/{id}", forbid)
		s := newMarkerSession(forbiddenServer(t, mux), 7)
		if err := s.DeleteMarkerComment(t.Context(), testMarker); err != nil {
			t.Errorf("DeleteMarkerComment: got error = %v, want = nil (403 must be tolerated)", err)
		}
	})

	t.Run("non-403 errors still surface", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/issues/{number}/comments", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		})
		s := newMarkerSession(forbiddenServer(t, mux), 7)
		if err := s.UpsertMarkerComment(t.Context(), testMarker, "blocked on foo"); err == nil {
			t.Error("UpsertMarkerComment: got error = nil, want non-nil for a 500 (only 403 is tolerated)")
		}
	})
}

func forbiddenServer(t *testing.T, mux *http.ServeMux) *github.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	return client
}

// TestApplyReadyForReview verifies the label is added only when absent: the API
// is called on the first green pass and is a no-op once the label is present.
func TestApplyReadyForReview(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("{{.PackageName}}"))
	bodyTmpl := template.Must(template.New("body").Parse("{{.PackageName}}"))
	cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl)
	if err != nil {
		t.Fatalf("creating CM: %v", err)
	}

	tests := []struct {
		name        string
		labels      []string
		wantAPICall bool
	}{{
		name:        "newly applied",
		labels:      nil,
		wantAPICall: true,
	}, {
		name:        "already labeled",
		labels:      []string{"test-bot/ready-for-review"},
		wantAPICall: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var apiCalled bool
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/issues/{number}/labels", func(w http.ResponseWriter, _ *http.Request) {
				apiCalled = true
				writeJSON(t, w, []*github.Label{})
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}

			s := &Session[testData]{
				manager:  cm,
				client:   client,
				owner:    "test-owner",
				repo:     "test-repo",
				prNumber: 7,
				prURL:    "https://example.test/pull/7",
				prLabels: tt.labels,
			}

			if _, err := s.ApplyReadyForReview(t.Context()); err != nil {
				t.Fatalf("ApplyReadyForReview: got error = %v, want = nil", err)
			}
			if apiCalled != tt.wantAPICall {
				t.Errorf("label API called: got = %v, want = %v", apiCalled, tt.wantAPICall)
			}
		})
	}
}

// TestFindMarkerCommentPaginates verifies the marker is found on a later page,
// exercising the pagination loop in findMarkerComment.
func TestFindMarkerCommentPaginates(t *testing.T) {
	page1 := []*github.IssueComment{{ID: github.Ptr(int64(1)), Body: github.Ptr("unrelated")}}
	page2 := []*github.IssueComment{{ID: github.Ptr(int64(2)), Body: github.Ptr(testMarker + "\nblocked on foo")}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(t, w, page2)
			return
		}
		w.Header().Set("Link", `<`+r.URL.Path+`?page=2>; rel="next"`)
		writeJSON(t, w, page1)
	})
	deleted := []int64{}
	mux.HandleFunc("DELETE /api/v3/repos/test-owner/test-repo/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleted = append(deleted, mustParseID(t, r.PathValue("id")))
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	s := newMarkerSession(client, 7)
	if err := s.DeleteMarkerComment(t.Context(), testMarker); err != nil {
		t.Fatalf("DeleteMarkerComment: got error = %v, want = nil", err)
	}
	if len(deleted) != 1 || deleted[0] != 2 {
		t.Errorf("deleted comments: got = %v, want = [2] (marker on page 2)", deleted)
	}
}
