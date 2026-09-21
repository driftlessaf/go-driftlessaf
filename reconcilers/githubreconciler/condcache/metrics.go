/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package condcache

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// mRequests counts GETs through the Transport by what the revalidation found.
//
// The "hit" share is the only direct measure of what this package saves. A
// hit is a 304, which the instrumented transport below records as an ordinary
// request and which never appears as a saving in the GitHub request log —
// the call still happens, it just does not draw on the rate limit. Nothing
// else reports that distinction.
var mRequests = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "github_conditional_requests_total",
		Help: "GitHub GETs through the conditional-request cache, by revalidation outcome.",
	},
	[]string{"result"}, // hit | miss | changed
)
