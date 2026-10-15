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
	// triggerRepass counts passes run in addition to the admitted trigger:
	// one after a shed trigger, or one after a pass that hit its batch cap.
	triggerRepass = "repass"
)

var mTriggers = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "workqueue_dispatch_triggers_total",
		Help: "The number of dispatch trigger requests by result, plus extra passes under repass: dispatched (admitted to initiate a pass), shed (dropped by the dispatch rate limit), or repass (not a request: an extra pass run at the next limiter slot after a shed trigger or a batch-capped pass).",
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
