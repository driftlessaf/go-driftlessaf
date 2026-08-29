/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"github.com/google/go-cmp/cmp"
)

func TestLegacyVertexDispatchPreservesModelLabelsAndTypedOptions(t *testing.T) {
	t.Parallel()

	claudeOption := claudeexecutor.WithTemperature[*Request, *Judgement](0.7)
	googleOption := googleexecutor.WithTemperature[*Request, *Judgement](0.7)

	t.Run("Claude", func(t *testing.T) {
		t.Parallel()
		labels := map[string]string{"workload": "judge"}
		var claudeCalls, googleCalls int
		constructors := legacyJudgeConstructors{
			claude: func(_ context.Context, projectID, region, modelName string, gotLabels map[string]string, opts ...claudeexecutor.Option[*Request, *Judgement]) (Interface, error) {
				claudeCalls++
				if projectID != "project-id" || region != "us-central1" || modelName != "ClAuDe-Sonnet-4-6" {
					t.Errorf("Claude constructor args = project:%q region:%q model:%q", projectID, region, modelName)
				}
				wantLabels := map[string]string{"workload": "judge", "model_name": "claude-sonnet-4-6"}
				if diff := cmp.Diff(wantLabels, gotLabels); diff != "" {
					t.Errorf("Claude labels (-want +got):\n%s", diff)
				}
				if len(opts) != 1 {
					t.Errorf("Claude typed options = %d, want 1", len(opts))
				}
				return &claude{}, nil
			},
			google: func(context.Context, string, string, string, map[string]string, ...googleexecutor.Option[*Request, *Judgement]) (Interface, error) {
				googleCalls++
				return &google{}, nil
			},
		}
		got, err := newVertexWithLabels(t.Context(), "project-id", "us-central1", "ClAuDe-Sonnet-4-6", labels, constructors, claudeOption, googleOption, "ignored")
		if err != nil {
			t.Fatalf("newVertexWithLabels: %v", err)
		}
		if _, ok := got.(*claude); !ok {
			t.Fatalf("constructed judge = %T, want Claude", got)
		}
		if claudeCalls != 1 || googleCalls != 0 {
			t.Fatalf("constructor calls = Claude:%d Google:%d, want 1,0", claudeCalls, googleCalls)
		}
		if _, ok := labels["model_name"]; ok {
			t.Errorf("caller labels were mutated: %v", labels)
		}
	})

	t.Run("Google", func(t *testing.T) {
		t.Parallel()
		labels := map[string]string{"workload": "judge"}
		var claudeCalls, googleCalls int
		constructors := legacyJudgeConstructors{
			claude: func(context.Context, string, string, string, map[string]string, ...claudeexecutor.Option[*Request, *Judgement]) (Interface, error) {
				claudeCalls++
				return &claude{}, nil
			},
			google: func(_ context.Context, projectID, region, modelName string, gotLabels map[string]string, opts ...googleexecutor.Option[*Request, *Judgement]) (Interface, error) {
				googleCalls++
				if projectID != "project-id" || region != "global" || modelName != "GeMiNi-2.5-Flash" {
					t.Errorf("Google constructor args = project:%q region:%q model:%q", projectID, region, modelName)
				}
				wantLabels := map[string]string{"workload": "judge", "model_name": "gemini-2.5-flash"}
				if diff := cmp.Diff(wantLabels, gotLabels); diff != "" {
					t.Errorf("Google labels (-want +got):\n%s", diff)
				}
				if len(opts) != 1 {
					t.Errorf("Google typed options = %d, want 1", len(opts))
				}
				return &google{}, nil
			},
		}
		got, err := newVertexWithLabels(t.Context(), "project-id", "global", "GeMiNi-2.5-Flash", labels, constructors, claudeOption, googleOption, 42)
		if err != nil {
			t.Fatalf("newVertexWithLabels: %v", err)
		}
		if _, ok := got.(*google); !ok {
			t.Fatalf("constructed judge = %T, want Google", got)
		}
		if claudeCalls != 0 || googleCalls != 1 {
			t.Fatalf("constructor calls = Claude:%d Google:%d, want 0,1", claudeCalls, googleCalls)
		}
		if _, ok := labels["model_name"]; ok {
			t.Errorf("caller labels were mutated: %v", labels)
		}
	})
}

func TestNewVertexPreservesUnsupportedModelError(t *testing.T) {
	t.Parallel()

	_, err := NewVertex(t.Context(), "project-id", "us-central1", "unsupported-model")
	if err == nil || !strings.Contains(err.Error(), "expected claude-* or gemini-*") {
		t.Fatalf("NewVertex error = %v, want legacy unsupported-model error", err)
	}
}
