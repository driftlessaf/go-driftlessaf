/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestListTeamIssues_PaginatesAndFilters(t *testing.T) {
	// Two pages: the first advertises a next cursor, the second closes it out.
	pages := []string{
		`{"data":{"issues":{"nodes":[
			{"id":"i1","identifier":"SEC-1","title":"[depthfirst] a"}
		],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}`,
		`{"data":{"issues":{"nodes":[
			{"id":"i2","identifier":"SEC-2","title":"[depthfirst] b"}
		],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`,
	}

	var gotVars []map[string]any
	var call int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		gotVars = append(gotVars, req.Variables)

		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, pages[call]); err != nil {
			t.Errorf("write response: %v", err)
		}
		call++
	}))
	defer srv.Close()

	c := NewClientWithAPIKey("lin_api_test").WithEndpoint(srv.URL)
	issues, err := c.ListTeamIssues(t.Context(), "SEC", ListTeamIssuesOptions{TitleContains: "[depthfirst]"})
	if err != nil {
		t.Fatalf("ListTeamIssues: %v", err)
	}

	if got, want := len(issues), 2; got != want {
		t.Fatalf("issue count: got = %d, want = %d", got, want)
	}
	if got, want := issues[0].Identifier, "SEC-1"; got != want {
		t.Errorf("issues[0].Identifier: got = %q, want = %q", got, want)
	}
	if got, want := issues[1].Identifier, "SEC-2"; got != want {
		t.Errorf("issues[1].Identifier: got = %q, want = %q", got, want)
	}

	// The first request must carry the team + title + open-state filter and no
	// cursor; the second must forward the first page's endCursor.
	if got, want := len(gotVars), 2; got != want {
		t.Fatalf("request count: got = %d, want = %d", got, want)
	}
	filter, ok := gotVars[0]["filter"].(map[string]any)
	if !ok {
		t.Fatalf("first request missing filter: %v", gotVars[0])
	}
	if _, ok := filter["title"]; !ok {
		t.Errorf("first request filter missing title clause: %v", filter)
	}
	if _, ok := filter["team"]; !ok {
		t.Errorf("first request filter missing team clause: %v", filter)
	}
	if _, ok := filter["state"]; !ok {
		t.Errorf("first request filter missing state clause: %v", filter)
	}
	// Presence alone isn't enough. The state clause is what keeps a sweep of
	// open issues free of completed and canceled ones, so pin its shape. The
	// wanted value is spelled out rather than built from closedStateTypes, so
	// that dropping a type from that list fails here instead of moving both
	// sides of the comparison together.
	//
	// filter came out of json.Unmarshal, so the clause is already decoded:
	// compare it structurally against the shape it must have. The nin list
	// decodes to []any of strings, not []string.
	wantState := map[string]any{"type": map[string]any{"nin": []any{"completed", "canceled"}}}
	if diff := cmp.Diff(wantState, filter["state"]); diff != "" {
		t.Errorf("first request state clause (-want +got):\n%s", diff)
	}
	if _, ok := gotVars[0]["after"]; ok {
		t.Errorf("first request should not send an after cursor: %v", gotVars[0])
	}
	if got, want := gotVars[1]["after"], "cursor-1"; got != want {
		t.Errorf("second request after cursor: got = %v, want = %q", got, want)
	}
}

// The project option must add a server-side project clause to the filter, and
// its absence (the zero value) must leave the filter without one — otherwise a
// sweep meant for a whole team would silently be narrowed to, or by, a project.
func TestListTeamIssues_ProjectClause(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts ListTeamIssuesOptions
		want any // nil means the clause must be absent
	}{{
		name: "set",
		opts: ListTeamIssuesOptions{Project: "Depthfirst Q3"},
		want: map[string]any{"name": map[string]any{"eq": "Depthfirst Q3"}},
	}, {
		name: "unset",
		opts: ListTeamIssuesOptions{},
		want: nil,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var gotFilter map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req struct {
					Variables map[string]any `json:"variables"`
				}
				if err := json.Unmarshal(body, &req); err != nil {
					t.Errorf("unmarshal request: %v", err)
				}
				gotFilter, _ = req.Variables["filter"].(map[string]any)
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, listIssuesResponse); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer srv.Close()

			c := NewClientWithAPIKey("lin_api_test").WithEndpoint(srv.URL)
			if _, err := c.ListTeamIssues(t.Context(), "SEC", tc.opts); err != nil {
				t.Fatalf("ListTeamIssues: %v", err)
			}
			if diff := cmp.Diff(tc.want, gotFilter["project"]); diff != "" {
				t.Errorf("project clause (-want +got):\n%s", diff)
			}
		})
	}
}

func TestListTeamIssues_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"errors":[{"message":"boom"}]}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	c := NewClientWithAPIKey("lin_api_test").WithEndpoint(srv.URL)
	if _, err := c.ListTeamIssues(t.Context(), "SEC", ListTeamIssuesOptions{}); err == nil {
		t.Fatal("expected error from GraphQL error response, got nil")
	}
}

// captureQuery runs call against a server that records the GraphQL query it is
// sent and answers with body.
func captureQuery(t *testing.T, body string, call func(*Client) error) string {
	t.Helper()

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(reqBody, &req); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		gotQuery = req.Query

		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	if err := call(NewClientWithAPIKey("lin_api_test").WithEndpoint(srv.URL)); err != nil {
		t.Fatalf("call: %v", err)
	}
	return gotQuery
}

const (
	listIssuesResponse = `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`
	getIssueResponse   = `{"data":{"issue":{"id":"i1","identifier":"SEC-1"}}}`
)

func listTeamIssuesQuery(t *testing.T) string {
	t.Helper()
	return captureQuery(t, listIssuesResponse, func(c *Client) error {
		_, err := c.ListTeamIssues(t.Context(), "SEC", ListTeamIssuesOptions{})
		return err
	})
}

// Reconciler state lives in an attachment, and both issue queries are
// documented as returning enough for a caller to load it. Left implicit, the
// attachments connection takes Linear's default page size of 50 and silently
// truncates, which reads as "no state" and makes a reconciler redo work it
// already did on every reconcile. Assert the page size is explicit in both
// queries so it cannot regress to the default.
func TestIssueQueries_BoundAttachmentsExplicitly(t *testing.T) {
	for _, tc := range []struct {
		name          string
		query         string
		wantSelection string
	}{
		{name: "ListTeamIssues", query: listTeamIssuesQuery(t), wantSelection: listAttachmentsSelection},
		{
			name: "GetIssue",
			query: captureQuery(t, getIssueResponse, func(c *Client) error {
				_, err := c.GetIssue(t.Context(), "i1")
				return err
			}),
			wantSelection: attachmentsSelection,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.query, tc.wantSelection) {
				t.Errorf("query does not request the bounded attachments connection %q\n---\n%s", tc.wantSelection, tc.query)
			}
			if strings.Contains(tc.query, "attachments {") {
				t.Errorf("query requests attachments with no page size, so Linear's default of 50 applies\n---\n%s", tc.query)
			}
		})
	}
}

// The list query asks for a page of issues, so its nested attachments
// connection multiplies out — and GraphQL cost is scored on what a query asks
// for, not on what comes back. Sharing the wide single-issue selection here
// would request issuesPageSize × 250 nodes per page, so assert the two stay
// distinct and that the list query carries the narrower one.
func TestListTeamIssues_DoesNotShareTheWideAttachmentsPage(t *testing.T) {
	if attachmentsSelection == listAttachmentsSelection {
		t.Fatal("the list query must not reuse the single-issue attachments page size")
	}
	query := listTeamIssuesQuery(t)
	if strings.Contains(query, attachmentsSelection) {
		t.Errorf("list query requests the wide (single-issue) attachments page\n---\n%s", query)
	}
	if !strings.Contains(query, "first: "+issuesPageSize+",") {
		t.Errorf("list query does not carry the expected issues page size %q\n---\n%s", issuesPageSize, query)
	}
}

// Cursor pagination under Linear's default updatedAt ordering silently drops an
// issue touched while a later page is in flight: it jumps ahead of the cursor
// and shifts the tail down. A sibling reconciler writing a state attachment is
// enough to trigger it, so the query must pin a stable ordering.
func TestListTeamIssues_OrdersByCreatedAtForStablePagination(t *testing.T) {
	if query := listTeamIssuesQuery(t); !strings.Contains(query, "orderBy: createdAt") {
		t.Errorf("list query must order by createdAt so the cursor is stable\n---\n%s", query)
	}
}

// An empty prefix must not be taken literally: "_state" reads as "no state"
// against anything already written under a real prefix, so a caller passing an
// unset environment variable would orphan its own state.
func TestWithStatePrefix_IgnoresEmpty(t *testing.T) {
	c := NewClientWithAPIKey("lin_api_test")
	want := c.stateAttachmentTitle()

	if got := c.WithStatePrefix("").stateAttachmentTitle(); got != want {
		t.Errorf("empty prefix: got = %q, want = %q (the default)", got, want)
	}
	if got, want := c.WithStatePrefix("tidy-upper").stateAttachmentTitle(), "tidy-upper_state"; got != want {
		t.Errorf("set prefix: got = %q, want = %q", got, want)
	}
}
