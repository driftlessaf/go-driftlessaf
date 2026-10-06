/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/graphqlclient"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v88/github"
)

// TestNewSession_CheckSource pins which connection GetPRInfo reads checks
// through. The default must stay statusCheckRollup with no checkSuiteFilter
// variable, so an existing reconciler's token permissions and its test fakes
// keep working; WithCheckSuites switches to checkSuites, which needs no
// statuses permission.
func TestNewSession_CheckSource(t *testing.T) {
	tests := []struct {
		name       string
		opts       []Option[testData]
		wantField  string
		avoidField string
		wantFilter any
	}{{
		name:       "default reads the rollup",
		wantField:  "statusCheckRollup",
		avoidField: "checkSuites",
	}, {
		name:       "check suites for every app",
		opts:       []Option[testData]{WithCheckSuites[testData]()},
		wantField:  "checkSuites(first: 100, filterBy: $checkSuiteFilter)",
		avoidField: "statusCheckRollup",
		wantFilter: nil,
	}, {
		name:       "check suites for one app",
		opts:       []Option[testData]{WithCheckSuites[testData](15368)},
		wantField:  "checkSuites(first: 100, filterBy: $checkSuiteFilter)",
		avoidField: "statusCheckRollup",
		wantFilter: map[string]any{"appId": float64(15368)},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decoding request body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data": {"repository": {"pullRequests": {"nodes": []}}}}`)
			})}}))
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}
			cm, err := New[testData]("test-bot", template.Must(template.New("t").Parse("t")), template.Must(template.New("b").Parse("b")), tc.opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := cm.NewSession(t.Context(), gh, &githubreconciler.Resource{Type: githubreconciler.ResourceTypeIssue, Owner: "o", Repo: "r", Number: 1}); err != nil {
				t.Fatalf("NewSession: %v", err)
			}

			if !strings.Contains(body.Query, tc.wantField) {
				t.Errorf("query %q: missing %q", body.Query, tc.wantField)
			}
			if strings.Contains(body.Query, tc.avoidField) {
				t.Errorf("query %q: contains %q", body.Query, tc.avoidField)
			}
			filter, ok := body.Variables["checkSuiteFilter"]
			if wantOK := tc.avoidField != "checkSuites"; ok != wantOK {
				t.Errorf("checkSuiteFilter variable present: got %v, want %v", ok, wantOK)
			}
			if diff := cmp.Diff(tc.wantFilter, filter); diff != "" {
				t.Errorf("checkSuiteFilter (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCollectSuiteFindings locks in that check runs read through check suites
// are classified as rollup contexts are (see TestCollectFindings): a FAILURE
// conclusion becomes a finding, a not-yet-complete status becomes a pending
// check, and success/neutral/cancelled and ignored runs are skipped.
func TestCollectSuiteFindings(t *testing.T) {
	cr := func(id int64, name, status, conclusion string) gqlCheckRunNode {
		return gqlCheckRunNode{
			DatabaseId: id,
			Name:       name,
			Status:     status,
			Conclusion: conclusion,
			DetailsUrl: "https://ci/" + name,
		}
	}
	suite := func(id string, runs ...gqlCheckRunNode) gqlCheckSuiteNode {
		s := gqlCheckSuiteNode{Id: id}
		s.CheckRuns.Nodes = runs
		return s
	}

	suites := gqlCheckSuitesConnection{
		Nodes: []gqlCheckSuiteNode{
			suite("s1",
				cr(1, "build", "COMPLETED", "FAILURE"),
				cr(2, "unit-tests", "IN_PROGRESS", ""),
				cr(3, "lint", "QUEUED", ""),
				cr(4, "vet", "COMPLETED", "SUCCESS"),
			),
			suite("s2",
				cr(5, "sbom", "COMPLETED", "NEUTRAL"),
				cr(6, "flaky", "COMPLETED", "CANCELLED"), // not FAILURE -> not a finding
				cr(7, "eligibility", "COMPLETED", "FAILURE"),
				cr(8, "eligibility-rerun", "IN_PROGRESS", ""),
			),
		},
		// No connection has a next page, so nothing is fetched and the nil
		// gqlClient is safe.
	}
	// Ignored runs are neither findings nor pending, whatever they conclude.
	ignored := map[string]struct{}{"eligibility": {}, "eligibility-rerun": {}}

	findings, pending, err := collectSuiteFindings(t.Context(), nil, "owner", "repo", "sha", nil, suites, ignored)
	if err != nil {
		t.Fatalf("collectSuiteFindings: %v", err)
	}

	wantFindings := []callbacks.Finding{{
		Kind:       callbacks.FindingKindCICheck,
		Identifier: "1",
		Name:       "build",
		Details:    formatCheckRunDetails("build", "COMPLETED", "FAILURE", "", "", "", "https://ci/build"),
		DetailsURL: "https://ci/build",
	}}
	if diff := cmp.Diff(wantFindings, findings); diff != "" {
		t.Errorf("findings (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"unit-tests", "lint"}, pending); diff != "" {
		t.Errorf("pendingChecks (-want +got):\n%s", diff)
	}
}

// checkRequest is the part of a GraphQL request a check-pagination test
// asserts on.
type checkRequest struct {
	Operation string
	Cursor    *string
	AppID     *int64
	SuiteID   string
}

// newCheckServer serves GraphQL requests from responses, keyed by the
// collectSuiteFindings query (PaginateCheckSuites or PaginateCheckRuns) and then by
// cursor ("" for none), and records each request.
func newCheckServer(t *testing.T, responses map[string]map[string]string) (*graphqlclient.GraphQLClient, *[]checkRequest) {
	t.Helper()
	var got []checkRequest
	client := newTestGraphQLClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string `json:"query"`
			Variables struct {
				Cursor           *string `json:"cursor"`
				SuiteID          string  `json:"suiteId"`
				CheckSuiteFilter *struct {
					AppID *int64 `json:"appId"`
				} `json:"checkSuiteFilter"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		// The operation name is only a metrics label, so it is not sent; tell
		// the two queries apart by the connection they read.
		op := "PaginateCheckSuites"
		if strings.Contains(body.Query, "node(id:") {
			op = "PaginateCheckRuns"
		}
		req := checkRequest{Operation: op, Cursor: body.Variables.Cursor, SuiteID: body.Variables.SuiteID}
		if f := body.Variables.CheckSuiteFilter; f != nil {
			req.AppID = f.AppID
		}
		got = append(got, req)

		resp, ok := responses[op][ptrOrEmpty(body.Variables.Cursor)]
		if !ok {
			t.Errorf("unexpected request: %+v", req)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, resp); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	return client, &got
}

func ptrOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// checkRunJSON renders one check run node of a test GraphQL response.
func checkRunJSON(id int, name, status, conclusion string) string {
	return fmt.Sprintf(`{"databaseId": %d, "name": %q, "status": %q, "conclusion": %q, "detailsUrl": "", "title": "", "summary": "", "text": ""}`, id, name, status, conclusion)
}

// suitesPageJSON renders a PaginateCheckSuites response.
func suitesPageJSON(nextCursor string, suites ...string) string {
	return fmt.Sprintf(`{"data": {"repository": {"object": {"checkSuites": {
	  "pageInfo": {"hasNextPage": %t, "endCursor": %q}, "nodes": [%s]}}}}}`,
		nextCursor != "", nextCursor, strings.Join(suites, ","))
}

// suiteJSON renders one check suite node whose runs have no further page.
func suiteJSON(id string, runs ...string) string {
	return fmt.Sprintf(`{"id": %q, "checkRuns": {"pageInfo": {"hasNextPage": false, "endCursor": ""}, "nodes": [%s]}}`,
		id, strings.Join(runs, ","))
}

// TestCollectSuiteFindings_Paginates drives collectSuiteFindings through a suite whose
// runs span two further pages and a further page of suites, and verifies that
// findings and pending checks merge across all of them and that each request
// carries the previous page's end cursor and the unfiltered app selection.
func TestCollectSuiteFindings_Paginates(t *testing.T) {
	gqlClient, got := newCheckServer(t, map[string]map[string]string{
		"PaginateCheckRuns": {
			"runs-1": `{"data": {"node": {"checkRuns": {
			  "pageInfo": {"hasNextPage": true, "endCursor": "runs-2"},
			  "nodes": [` + checkRunJSON(3, "integration", "COMPLETED", "FAILURE") + `,` + checkRunJSON(4, "docs", "COMPLETED", "SUCCESS") + `]}}}}`,
			"runs-2": `{"data": {"node": {"checkRuns": {
			  "pageInfo": {"hasNextPage": false, "endCursor": ""},
			  "nodes": [` + checkRunJSON(5, "e2e", "QUEUED", "") + `]}}}}`,
		},
		"PaginateCheckSuites": {
			"suites-1": suitesPageJSON("", suiteJSON("s2", checkRunJSON(6, "release", "COMPLETED", "FAILURE"))),
		},
	})

	var initial gqlCheckSuitesConnection
	s1 := gqlCheckSuiteNode{Id: "s1"}
	s1.CheckRuns.Nodes = []gqlCheckRunNode{
		{DatabaseId: 1, Name: "build", Status: "COMPLETED", Conclusion: "FAILURE"},
		{DatabaseId: 2, Name: "unit-tests", Status: "IN_PROGRESS"},
	}
	s1.CheckRuns.PageInfo.HasNextPage = true
	s1.CheckRuns.PageInfo.EndCursor = "runs-1"
	initial.Nodes = []gqlCheckSuiteNode{s1}
	initial.PageInfo.HasNextPage = true
	initial.PageInfo.EndCursor = "suites-1"

	findings, pending, err := collectSuiteFindings(t.Context(), gqlClient, "owner", "repo", "sha", nil, initial, nil)
	if err != nil {
		t.Fatalf("collectSuiteFindings: %v", err)
	}

	var findingNames []string
	for _, f := range findings {
		findingNames = append(findingNames, f.Name)
	}
	if diff := cmp.Diff([]string{"build", "integration", "release"}, findingNames); diff != "" {
		t.Errorf("findings (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"unit-tests", "e2e"}, pending); diff != "" {
		t.Errorf("pendingChecks (-want +got):\n%s", diff)
	}
	wantRequests := []checkRequest{
		{Operation: "PaginateCheckRuns", Cursor: new("runs-1"), SuiteID: "s1"},
		{Operation: "PaginateCheckRuns", Cursor: new("runs-2"), SuiteID: "s1"},
		{Operation: "PaginateCheckSuites", Cursor: new("suites-1")},
	}
	if diff := cmp.Diff(wantRequests, *got); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

// TestCollectSuiteFindings_CheckApps verifies that with several apps configured,
// the initial suites (the first app's) are used as given and each further
// app's suites are fetched from the start with that app's filter, including
// later pages, which keep the filter.
func TestCollectSuiteFindings_CheckApps(t *testing.T) {
	gqlClient, got := newCheckServer(t, map[string]map[string]string{
		"PaginateCheckSuites": {
			"":         suitesPageJSON("suites-1", suiteJSON("s2", checkRunJSON(2, "external", "COMPLETED", "FAILURE"))),
			"suites-1": suitesPageJSON("", suiteJSON("s3", checkRunJSON(3, "external-slow", "IN_PROGRESS", ""))),
		},
	})

	var initial gqlCheckSuitesConnection
	s1 := gqlCheckSuiteNode{Id: "s1"}
	s1.CheckRuns.Nodes = []gqlCheckRunNode{{DatabaseId: 1, Name: "build", Status: "COMPLETED", Conclusion: "FAILURE"}}
	initial.Nodes = []gqlCheckSuiteNode{s1}

	findings, pending, err := collectSuiteFindings(t.Context(), gqlClient, "owner", "repo", "sha", []int64{15368, 42}, initial, nil)
	if err != nil {
		t.Fatalf("collectSuiteFindings: %v", err)
	}

	var findingNames []string
	for _, f := range findings {
		findingNames = append(findingNames, f.Name)
	}
	if diff := cmp.Diff([]string{"build", "external"}, findingNames); diff != "" {
		t.Errorf("findings (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"external-slow"}, pending); diff != "" {
		t.Errorf("pendingChecks (-want +got):\n%s", diff)
	}
	wantRequests := []checkRequest{
		{Operation: "PaginateCheckSuites", AppID: new(int64(42))},
		{Operation: "PaginateCheckSuites", Cursor: new("suites-1"), AppID: new(int64(42))},
	}
	if diff := cmp.Diff(wantRequests, *got); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

// TestWithCheckSuites verifies that the option enables check suites and that
// duplicate app IDs collapse, so an app's suites are not fetched (and its
// findings reported) twice.
func TestWithCheckSuites(t *testing.T) {
	cm, err := New[testData]("test-bot", template.Must(template.New("t").Parse("t")), template.Must(template.New("b").Parse("b")),
		WithCheckSuites[testData](15368, 42, 15368))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !cm.checkSuites {
		t.Error("checkSuites: got false, want true")
	}
	if diff := cmp.Diff([]int64{15368, 42}, cm.checkApps); diff != "" {
		t.Errorf("checkApps (-want +got):\n%s", diff)
	}
}

// TestCollectSuiteFindings_PaginationErrorPropagates verifies that a failed
// suite page fails collectSuiteFindings instead of silently truncating
// findings, which would let a red or pending PR read as green downstream.
func TestCollectSuiteFindings_PaginationErrorPropagates(t *testing.T) {
	gqlClient := newTestGraphQLClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))

	var initial gqlCheckSuitesConnection
	s1 := gqlCheckSuiteNode{Id: "s1"}
	s1.CheckRuns.Nodes = []gqlCheckRunNode{{DatabaseId: 1, Name: "build", Status: "COMPLETED", Conclusion: "FAILURE"}}
	initial.Nodes = []gqlCheckSuiteNode{s1}
	initial.PageInfo.HasNextPage = true
	initial.PageInfo.EndCursor = "cursor-1"

	findings, pending, err := collectSuiteFindings(t.Context(), gqlClient, "owner", "repo", "sha", nil, initial, nil)
	if err == nil {
		t.Fatal("collectSuiteFindings: got nil error, want pagination error")
	}
	if want := "paginating check suites"; !strings.Contains(err.Error(), want) {
		t.Errorf("error: got %q, want containing %q", err, want)
	}
	if findings != nil || pending != nil {
		t.Errorf("partial results returned alongside error: findings=%v pending=%v", findings, pending)
	}
}
