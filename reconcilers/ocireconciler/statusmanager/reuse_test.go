//go:build withauth

/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager_test

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/ocireconciler/statusmanager"
	statusmanagertesting "chainguard.dev/driftlessaf/reconcilers/ocireconciler/statusmanager/testing"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

// countingTransport counts requests to the registry version-check endpoint
// ("/v2/"), which ggcr performs once per fresh transport setup. A shared
// Puller sets its transport up once for the life of the Manager, so the ping
// count distinguishes reused options from clean-slate options.
type countingTransport struct {
	inner http.RoundTripper
	pings atomic.Int32
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/v2/" {
		c.pings.Add(1)
	}
	return c.inner.RoundTrip(req)
}

// TestRepositoryOverrideSharesPuller pins the reuse gate: a Manager with a
// repository override serves all reads through one shared Puller (a single
// registry ping across many sessions), while a Manager without an override
// keeps clean-slate options per call (a ping per read).
func TestRepositoryOverrideSharesPuller(t *testing.T) {
	ctx := t.Context()
	registryHost := setupTestRegistry(t)

	const reads = 3
	for _, tc := range []struct {
		name     string
		override bool
	}{
		{name: "override shares one puller", override: true},
		{name: "no override keeps clean-slate options", override: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct := &countingTransport{inner: http.DefaultTransport}
			opts := []statusmanager.Option{
				statusmanager.WithRemoteOptions(remote.WithTransport(ct)),
			}
			if tc.override {
				opts = append(opts, statusmanager.WithRepositoryOverride(registryHost+"/reuse-repo"))
			}
			mgr, err := statusmanagertesting.NewReadOnly[TestStatus](ctx, t, "test-reconciler", opts...)
			require.NoError(t, err)

			var afterFirst int32
			for i := range reads {
				digest, err := name.NewDigest(fmt.Sprintf(
					"%s/subject-%d@sha256:%064d", registryHost, i, i))
				require.NoError(t, err)
				observed, err := mgr.NewSession(digest).ObservedState(ctx)
				require.NoError(t, err)
				require.Nil(t, observed, "no status was written")
				if i == 0 {
					afterFirst = ct.pings.Load()
				}
			}

			// The first read pays the transport-setup pings either way; the
			// invariant under reuse is that later reads pay none.
			pings := ct.pings.Load()
			if tc.override {
				require.Equal(t, afterFirst, pings,
					"shared puller must not ping again after the first read")
			} else {
				require.Greater(t, pings, afterFirst, "clean-slate options ping per read")
			}
		})
	}
}
