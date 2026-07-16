/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/google/go-cmp/cmp"
)

// TestAppendUserPromptSuffix asserts the exact concatenation the executors
// perform on the built user prompt: nil suffix passes the prompt through
// unchanged, a bound suffix is appended with a blank-line separator, and an
// unbuildable suffix (unbound placeholder) surfaces as an error.
func TestAppendUserPromptSuffix(t *testing.T) {
	t.Parallel()

	suffix, err := promptbuilder.NewPrompt("lens suffix body")
	if err != nil {
		t.Fatalf("NewPrompt(suffix) error = %v", err)
	}
	unbuildable, err := promptbuilder.NewPrompt("{{unbound}}")
	if err != nil {
		t.Fatalf("NewPrompt(unbuildable) error = %v", err)
	}

	tests := []struct {
		name    string
		suffix  *promptbuilder.Prompt
		want    string
		wantErr bool
	}{
		{name: "nil suffix passes prompt through", suffix: nil, want: "changeset payload"},
		{name: "suffix appended with blank-line separator", suffix: suffix, want: "changeset payload\n\nlens suffix body"},
		{name: "unbuildable suffix returns error", suffix: unbuildable, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := AppendUserPromptSuffix("changeset payload", tt.suffix)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AppendUserPromptSuffix() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got != tt.want {
				t.Errorf("AppendUserPromptSuffix(): got = %q, want = %q", got, tt.want)
			}
		})
	}
}

// TestDefaultResourceLabels pins the environment-derived default behavior the
// executors share: values come from the deployment env vars, unset vars fall
// back to "unknown", and custom labels override defaults on key match while
// extra keys pass through. No t.Parallel: t.Setenv forbids it.
func TestDefaultResourceLabels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service string
		job     string
		product string
		team    string
		labels  map[string]string
		want    map[string]string
	}{
		{
			name: "all env unset falls back to unknown",
			want: map[string]string{"service_name": "unknown", "product": "unknown", "team": "unknown"},
		},
		{
			name:    "defaults derived from environment",
			service: "skillup-rec",
			product: "agents",
			team:    "dev-platform",
			want:    map[string]string{"service_name": "skillup-rec", "product": "agents", "team": "dev-platform"},
		},
		{
			name: "service name falls back to CLOUD_RUN_JOB",
			job:  "requeue-cron",
			want: map[string]string{"service_name": "requeue-cron", "product": "unknown", "team": "unknown"},
		},
		{
			name:    "custom labels override defaults and add keys",
			service: "skillup-rec",
			product: "agents",
			team:    "dev-platform",
			labels:  map[string]string{"team": "platform", "model": "claude"},
			want:    map[string]string{"service_name": "skillup-rec", "product": "agents", "team": "platform", "model": "claude"},
		},
		{
			name:   "custom labels merge over unknown defaults",
			labels: map[string]string{"product": "agents"},
			want:   map[string]string{"service_name": "unknown", "product": "agents", "team": "unknown"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("K_SERVICE", tc.service)
			t.Setenv("CLOUD_RUN_JOB", tc.job)
			t.Setenv("CHAINGUARD_PRODUCT", tc.product)
			t.Setenv("CHAINGUARD_TEAM", tc.team)

			got := DefaultResourceLabels(tc.labels)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("DefaultResourceLabels() mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}
