/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package schedtest_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/dag/internal/schedtest"
)

func TestSerialConformance(t *testing.T) {
	schedtest.RunConformance(t, schedtest.Serial)
}
