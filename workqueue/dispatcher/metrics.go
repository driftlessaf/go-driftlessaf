/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"cmp"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"chainguard.dev/driftlessaf/internal/cloudrun"
)

// metricServiceName and metricRevisionName label every dispatcher metric with
// the running Cloud Run resource's identity. They prefer the service
// variables (K_SERVICE/K_REVISION) and fall back to the job variables
// (CLOUD_RUN_JOB/CLOUD_RUN_EXECUTION), defaulting to "unknown".
var (
	metricServiceName  = cmp.Or(cloudrun.ServiceName(), "unknown")
	metricRevisionName = cmp.Or(cloudrun.RevisionName(), "unknown")
)

// Trigger results recorded by mTriggers.
const (
	triggerDispatched = "dispatched"
	triggerShed       = "shed"
)

var mTriggers = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "workqueue_dispatch_triggers_total",
		Help: "The number of dispatch trigger requests by result: dispatched (admitted to initiate a pass) or shed (dropped by the dispatch rate limit).",
	},
	[]string{"service_name", "revision_name", "result"},
)

// countTrigger records the disposition of one dispatch trigger request.
func countTrigger(result string) {
	mTriggers.With(prometheus.Labels{
		"service_name":  metricServiceName,
		"revision_name": metricRevisionName,
		"result":        result,
	}).Inc()
}
