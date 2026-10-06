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
// hit is a 304 replayed as a 200, so the instrumented transport above records
// it as an ordinary request; per request, only the cache field of its
// github_api_call log (see mark) tells it apart.
var mRequests = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "github_conditional_requests_total",
		Help: "GitHub GETs through the conditional-request cache, by revalidation outcome.",
	},
	[]string{"result"}, // hit | miss | changed
)

// mEvictions counts entries dropped to make room for another. A rate that
// tracks the miss rate means a client's working set exceeds its budget.
var mEvictions = promauto.NewCounter(prometheus.CounterOpts{
	Name: "github_conditional_cache_evictions_total",
	Help: "Conditional-request cache entries evicted to stay within a bound.",
})
