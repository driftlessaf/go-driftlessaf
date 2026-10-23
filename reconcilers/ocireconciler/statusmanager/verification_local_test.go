/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager_test

import (
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"testing"
)

type loopbackTransport struct{ inner http.RoundTripper }

func (tr loopbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	addr, err := netip.ParseAddr(req.URL.Hostname())
	if err != nil || !addr.IsLoopback() {
		return nil, fmt.Errorf("test forbids non-loopback HTTP host %q", req.URL.Hostname())
	}
	return tr.inner.RoundTrip(req)
}

func TestMain(m *testing.M) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	http.DefaultTransport = loopbackTransport{inner: transport}
	os.Exit(m.Run())
}
