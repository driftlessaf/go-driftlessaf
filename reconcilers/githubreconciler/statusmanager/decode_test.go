/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"testing"

	internaltemplate "chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/template"
	"github.com/google/go-cmp/cmp"
)

func TestDecodeSummaryProducerContract(t *testing.T) {
	type details struct {
		Profile string `json:"profile"`
	}
	producer, err := internaltemplate.New[Status[details]]("reviewbot", "-status", "status")
	if err != nil {
		t.Fatal(err)
	}
	want := &Status[details]{ObservedGeneration: "head", Status: "completed", Conclusion: "neutral", Details: details{Profile: "package-update"}}
	forged, err := producer.Embed("author-influenced text", &Status[details]{ObservedGeneration: "old", Status: "completed", Conclusion: "success"})
	if err != nil {
		t.Fatal(err)
	}
	published, err := producer.Embed(forged, want)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, summary string
		want          *Status[details]
		wantErr       bool
	}{
		{"authoritative final producer marker", published, want, false},
		{"missing state", "empty findings", nil, false},
		{"wrong identity", "<!--another-status--><!--{}--><!--/another-status-->", nil, false},
		{"corrupt final state", published + "<!--reviewbot-status--><!--{broken}--><!--/reviewbot-status-->", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeSummary[details]("reviewbot", tt.summary)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error: got %v, wantErr %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("state (-want,+got): %s", diff)
			}
			manager := &StatusManager[details]{identity: "reviewbot", templateExecutor: producer}
			managed, managedErr := manager.ExtractFromSummary(tt.summary)
			if (managedErr != nil) != tt.wantErr || cmp.Diff(tt.want, managed) != "" {
				t.Fatal("standalone and manager consumers disagree")
			}
		})
	}
}
