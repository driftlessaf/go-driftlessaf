/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudebackend_test

import (
	"context"

	"chainguard.dev/driftlessaf/agents/internal/claudebackend"
)

func ExampleResolve() {
	backend, err := claudebackend.Resolve(
		context.Background(),
		"my-project",
		"us-central1",
		"claude-sonnet-4-6@default",
	)
	if err != nil {
		return
	}
	_, _, _ = backend.Messages, backend.ModelID, backend.Provider
}
