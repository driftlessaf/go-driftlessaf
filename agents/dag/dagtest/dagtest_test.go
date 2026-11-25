/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dagtest_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/dag"
	"chainguard.dev/driftlessaf/agents/dag/dagtest"
)

func TestMemConformance(t *testing.T) {
	dagtest.RunResumerConformance(t, func(*testing.T) dag.Resumer { return dagtest.NewMem() })
}
