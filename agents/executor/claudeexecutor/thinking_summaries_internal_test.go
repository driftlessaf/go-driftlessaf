/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
)

// TestConfigDigestThinkingSummaries checks that summarized thinking joins the
// resume digest, so a run parked without it does not resume with it.
func TestConfigDigestThinkingSummaries(t *testing.T) {
	tools := map[string]claudetool.Metadata[*testResponse]{}
	opus := WithModel[*testBindable, *testResponse]("claude-opus-5-5")

	on, err := newTestExecutor(t, opus).configDigest(tools)
	if err != nil {
		t.Fatalf("configDigest: %v", err)
	}
	off, err := newTestExecutor(t, opus, WithThinkingSummaries[*testBindable, *testResponse](false)).configDigest(tools)
	if err != nil {
		t.Fatalf("configDigest: %v", err)
	}
	if on == off {
		t.Error("configDigest: got the same digest with and without thinking summaries, want different ones")
	}
}
