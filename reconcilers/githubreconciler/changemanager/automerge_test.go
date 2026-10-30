/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v88/github"
	"github.com/shurcooL/githubv4"
)

// TestEnableAutoMerge drives EnableAutoMerge against a GraphQL double that
// answers the enablePullRequestAutoMerge mutation with each response GitHub
// gives, checking the input it sends and how it reports each refusal.
func TestEnableAutoMerge(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     AutoMergeResult
		wantErr  bool
	}{{
		name:     "armed",
		response: `{"data":{"enablePullRequestAutoMerge":{"pullRequest":{"id":"PR_1"}}}}`,
		want:     AutoMergeArmed,
	}, {
		name:     "clean status is benign",
		response: `{"errors":[{"type":"UNPROCESSABLE","message":"Pull request Pull request is in clean status"}]}`,
		want:     AutoMergeRefusedClean,
	}, {
		name:     "unstable status is benign",
		response: `{"errors":[{"type":"UNPROCESSABLE","message":"Pull request Pull request is in unstable status"}]}`,
		want:     AutoMergeRefusedUnstable,
	}, {
		name:     "other refusal is an error",
		response: `{"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`,
		wantErr:  true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var inputs []map[string]any
			gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Query     string `json:"query"`
					Variables struct {
						Input map[string]any `json:"input"`
					} `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decoding request body: %v", err)
				}
				if !strings.Contains(body.Query, "enablePullRequestAutoMerge") {
					t.Errorf("query: got = %q, want the enablePullRequestAutoMerge mutation", body.Query)
				}
				inputs = append(inputs, body.Variables.Input)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.response)
			})}}))
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}

			got, err := EnableAutoMerge(t.Context(), gh, githubv4.ID("PR_1"), githubv4.PullRequestMergeMethodSquash)

			if (err != nil) != tc.wantErr {
				t.Fatalf("EnableAutoMerge() error = %v, want error: %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("EnableAutoMerge() = %v, want %v", got, tc.want)
			}
			want := []map[string]any{{"pullRequestId": "PR_1", "mergeMethod": "SQUASH"}}
			if diff := gocmp.Diff(want, inputs); diff != "" {
				t.Errorf("mutation inputs (-want +got):\n%s", diff)
			}
		})
	}
}
