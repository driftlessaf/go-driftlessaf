/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue

import (
	"net/url"
	"testing"
)

func TestKeepaliveOptions(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     int
	}{{
		name:     "https cloud run target pings",
		endpoint: "https://reconciler-abc123-uc.a.run.app",
		want:     1,
	}, {
		name:     "https with explicit port pings",
		endpoint: "https://reconciler.example.com:8443",
		want:     1,
	}, {
		name:     "uppercase https scheme pings",
		endpoint: "HTTPS://reconciler-abc123-uc.a.run.app",
		want:     1,
	}, {
		name:     "http in-cluster target does not ping",
		endpoint: "http://reconciler.default.svc.cluster.local",
		want:     0,
	}, {
		name:     "http localhost target does not ping",
		endpoint: "http://localhost:8080",
		want:     0,
	}, {
		name:     "unix socket target does not ping",
		endpoint: "unix:///tmp/reconciler.sock",
		want:     0,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.endpoint)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", tt.endpoint, err)
			}
			if got := len(keepaliveOptions(u.Scheme)); got != tt.want {
				t.Errorf("len(keepaliveOptions(%q)): got = %d, want = %d", u.Scheme, got, tt.want)
			}
		})
	}
}
