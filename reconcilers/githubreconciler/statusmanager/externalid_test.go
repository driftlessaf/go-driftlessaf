/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"github.com/google/go-github/v88/github"
)

func TestExternalIDPublication(t *testing.T) {
	const metadata = `{"v":1,"identifier":"sandbox","trigger":"presubmit"}`
	var live github.CheckRun
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeSkipJSON(t, w, &github.ListCheckRunsResults{CheckRuns: []*github.CheckRun{&live}})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&live); err != nil {
			t.Errorf("decode %s check request = %v, want nil", r.Method, err)
		}
		live.ID = new(int64(1))
		writes++
		if live.GetExternalID() != metadata {
			t.Errorf("%s check external_id = %q, want %q", r.Method, live.GetExternalID(), metadata)
		}
		writeSkipJSON(t, w, &live)
	}))
	defer srv.Close()
	client, err := github.NewClient(github.WithHTTPClient(srv.Client()), github.WithEnterpriseURLs(srv.URL, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	sm := skipManager[TestDetails](t, true)
	cfg := &config{}
	WithExternalID(metadata)(cfg)
	sm.externalID = cfg.externalID
	res := &githubreconciler.Resource{Owner: skipOwner, Repo: skipRepo, Type: githubreconciler.ResourceTypePullRequest, Number: 1}
	session := sm.NewSession(client, res, skipSHA)
	state := &Status[TestDetails]{Status: "queued"}
	if err := session.SetActualState(t.Context(), "Queued", state); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("SetActualState(queued) writes = %d, want 1", writes)
	}
	// Exercise the observed-state path as well as the same-session cache.
	session = sm.NewSession(client, res, skipSHA)
	if _, err := session.ObservedState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := session.SetActualState(t.Context(), "Queued", state); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Errorf("SetActualState(unchanged external_id) writes = %d, want 1", writes)
	}
	live.ExternalID = new("old metadata")
	session = sm.NewSession(client, res, skipSHA)
	if _, err := session.ObservedState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := session.SetActualState(t.Context(), "Queued", state); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Errorf("SetActualState(changed external_id) writes = %d, want 2", writes)
	}
	sm.externalID = ""
	session = sm.NewSession(client, res, skipSHA)
	if _, err := session.ObservedState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := session.SetActualState(t.Context(), "Queued", state); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Errorf("SetActualState(unmanaged external_id) writes = %d, want 2", writes)
	}
}
