/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
)

// TestConfigDigestWithoutSuspendOnSubmit: an executor without the option
// digests the static params alone, so envelopes from executors without the
// option keep resuming.
func TestConfigDigestWithoutSuspendOnSubmit(t *testing.T) {
	submit := WithSubmitResultProvider[*testBindable, *testResponse](submitProvider())
	tools := map[string]claudetool.Metadata[*testResponse]{}

	plain := newTestExecutor(t, submit)
	static, _, _, err := plain.buildStaticParams(tools, "")
	if err != nil {
		t.Fatalf("buildStaticParams: %v", err)
	}
	want, err := checkpoint.DigestJSON(static)
	if err != nil {
		t.Fatalf("DigestJSON: %v", err)
	}
	got, err := plain.configDigest(tools)
	if err != nil {
		t.Fatalf("configDigest: %v", err)
	}
	if got != want {
		t.Errorf("configDigest without the option: got %s, want the static params' digest %s", got, want)
	}

	suspending, err := newTestExecutor(t, submit, WithSuspendOnSubmit[*testBindable, *testResponse]()).configDigest(tools)
	if err != nil {
		t.Fatalf("configDigest: %v", err)
	}
	if suspending == want {
		t.Error("configDigest with the option: got the plain digest, want a different one")
	}
}
