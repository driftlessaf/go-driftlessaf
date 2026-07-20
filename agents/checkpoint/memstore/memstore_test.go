/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package memstore_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/memstore"
	"chainguard.dev/driftlessaf/agents/checkpoint/storetest"
)

func TestConformance(t *testing.T) {
	storetest.RunConformance(t, func() checkpoint.Store {
		return memstore.New()
	})
}
