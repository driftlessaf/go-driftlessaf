/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/google/go-github/v88/github"
)

// TestReplaceLabels verifies ReplaceLabels adds the desired labels, prunes
// stale labels only under declared prefixes, and keeps the cached label set
// accurate across calls. Membership is per set (several desired labels under
// one prefix all stay), a declared prefix with no desired label is skipped,
// and labels outside the declared prefixes are never removed.
func TestReplaceLabels(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("{{.PackageName}}"))
	bodyTmpl := template.Must(template.New("body").Parse("Update {{.PackageName}}"))

	// call is one ReplaceLabels invocation; cases with two calls model two
	// reconciles of the same session stamping different result-derived labels.
	type call struct {
		labels   []string
		prefixes []string
	}

	tests := []struct {
		name        string
		noPR        bool     // build the session with prNumber == 0 (no PR yet)
		prLabels    []string // labels already cached on the session
		calls       []call
		failRemove  []string // labels whose DELETE /labels/{name} returns 404
		wantErr     string   // substring every call's error must contain; empty means no error
		wantAdded   []string // union of labels sent via POST /labels across calls
		wantRemoved []string // labels removed via DELETE /labels/{name}
		wantCache   []string // labels expected in s.prLabels afterwards
	}{{
		name:     "stale pkg label replaced across two reconciles",
		prLabels: []string{"automated-pr"},
		calls: []call{
			{labels: []string{"bot:pkg:oldname"}, prefixes: []string{"bot:pkg:"}},
			{labels: []string{"bot:pkg:newname"}, prefixes: []string{"bot:pkg:"}},
		},
		wantAdded:   []string{"bot:pkg:oldname", "bot:pkg:newname"},
		wantRemoved: []string{"bot:pkg:oldname"},
		wantCache:   []string{"automated-pr", "bot:pkg:newname"},
	}, {
		name:     "stale lang label replaced across two reconciles",
		prLabels: []string{"automated-pr"},
		calls: []call{
			{labels: []string{"bot:lang:python"}, prefixes: []string{"bot:lang:"}},
			{labels: []string{"bot:lang:go"}, prefixes: []string{"bot:lang:"}},
		},
		wantAdded:   []string{"bot:lang:python", "bot:lang:go"},
		wantRemoved: []string{"bot:lang:python"},
		wantCache:   []string{"automated-pr", "bot:lang:go"},
	}, {
		name:     "several desired labels under one prefix all stay",
		prLabels: []string{"bot:hold:cve", "bot:hold:fips"},
		calls: []call{
			{labels: []string{"bot:hold:cve", "bot:hold:fips"}, prefixes: []string{"bot:hold:"}},
		},
		wantCache: []string{"bot:hold:cve", "bot:hold:fips"},
	}, {
		name:     "labels under an undeclared prefix are untouched",
		prLabels: []string{"bot:hold:cve", "bot:hold:fips", "bot:pkg:oldname"},
		calls: []call{
			{labels: []string{"bot:pkg:newname"}, prefixes: []string{"bot:pkg:"}},
		},
		wantAdded:   []string{"bot:pkg:newname"},
		wantRemoved: []string{"bot:pkg:oldname"},
		wantCache:   []string{"bot:hold:cve", "bot:hold:fips", "bot:pkg:newname"},
	}, {
		name:     "declared prefix with no desired label is skipped",
		prLabels: []string{"bot:pkg:oldname"},
		calls: []call{
			{labels: []string{"automated-pr"}, prefixes: []string{"bot:pkg:"}},
		},
		wantAdded: []string{"automated-pr"},
		wantCache: []string{"bot:pkg:oldname", "automated-pr"},
	}, {
		name:     "empty prefixes behaves like AddLabels",
		prLabels: []string{"bot:pkg:oldname"},
		calls: []call{
			{labels: []string{"bot:pkg:newname"}},
		},
		wantAdded: []string{"bot:pkg:newname"},
		wantCache: []string{"bot:pkg:oldname", "bot:pkg:newname"},
	}, {
		name:     "no PR is a no-op",
		noPR:     true,
		prLabels: []string{"bot:pkg:oldname"},
		calls: []call{
			{labels: []string{"bot:pkg:newname"}, prefixes: []string{"bot:pkg:"}},
		},
		wantCache: []string{"bot:pkg:oldname"},
	}, {
		name:     "unrelated labels survive the replace",
		prLabels: []string{"automated-pr", "agentic", "bot:pkg:oldname"},
		calls: []call{
			{labels: []string{"bot:pkg:newname"}, prefixes: []string{"bot:pkg:"}},
		},
		wantAdded:   []string{"bot:pkg:newname"},
		wantRemoved: []string{"bot:pkg:oldname"},
		wantCache:   []string{"automated-pr", "agentic", "bot:pkg:newname"},
	}, {
		name:     "a failed removal does not stop the remaining removals",
		prLabels: []string{"bot:pkg:old", "bot:lang:python"},
		calls: []call{
			{labels: []string{"bot:pkg:new", "bot:lang:go"}, prefixes: []string{"bot:pkg:", "bot:lang:"}},
		},
		failRemove:  []string{"bot:pkg:old"},
		wantErr:     "bot:pkg:old",
		wantAdded:   []string{"bot:pkg:new", "bot:lang:go"},
		wantRemoved: []string{"bot:lang:python"},
		wantCache:   []string{"bot:pkg:old", "bot:pkg:new", "bot:lang:go"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var added []string
			var removed []string

			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/issues/{number}/labels", func(w http.ResponseWriter, r *http.Request) {
				var payload []string
				if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
					added = append(added, payload...)
				}
				writeJSON(t, w, []*github.Label{})
			})
			mux.HandleFunc("DELETE /api/v3/repos/test-owner/test-repo/issues/{number}/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
				name := r.PathValue("name")
				if slices.Contains(tt.failRemove, name) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				removed = append(removed, name)
				writeJSON(t, w, []*github.Label{})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}

			cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl)
			if err != nil {
				t.Fatalf("creating CM: %v", err)
			}

			prNumber := 99
			if tt.noPR {
				prNumber = 0
			}
			session := &Session[testData]{
				manager:  cm,
				client:   client,
				owner:    "test-owner",
				repo:     "test-repo",
				prNumber: prNumber,
				prLabels: tt.prLabels,
			}

			for _, c := range tt.calls {
				err := session.ReplaceLabels(t.Context(), c.labels, c.prefixes)
				if tt.wantErr == "" {
					if err != nil {
						t.Fatalf("ReplaceLabels(%v, %v): got error = %v, wanted nil", c.labels, c.prefixes, err)
					}
					continue
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ReplaceLabels(%v, %v): got error = %v, want error containing %q", c.labels, c.prefixes, err, tt.wantErr)
				}
			}

			if !slicesEqualUnordered(added, tt.wantAdded) {
				t.Errorf("POST /labels payloads: got = %v, want = %v", added, tt.wantAdded)
			}
			if !slicesEqualUnordered(removed, tt.wantRemoved) {
				t.Errorf("DELETE /labels: got removed = %v, want = %v", removed, tt.wantRemoved)
			}
			if !slicesEqualUnordered(session.prLabels, tt.wantCache) {
				t.Errorf("session.prLabels after ReplaceLabels: got = %v, want = %v", session.prLabels, tt.wantCache)
			}
		})
	}
}
