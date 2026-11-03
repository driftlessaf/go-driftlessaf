/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// mUnparseableKeys counts keys dropped because ParseURL rejected them.
// Process answers the drop as a completed key, so the workqueue's own
// metrics record it as a success; this counter is the only place a drop
// shows up besides the warning log.
var mUnparseableKeys = promauto.NewCounter(prometheus.CounterOpts{
	Name: "github_reconciler_unparseable_keys_total",
	Help: "Workqueue keys dropped without retry because they are not GitHub resource URLs.",
})
