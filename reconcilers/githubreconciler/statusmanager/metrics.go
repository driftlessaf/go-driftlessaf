/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// mCheckRunWrites counts what SetActualState did with each call. The
// "unchanged" share is the only direct measure of what WithSkipUnchangedUpdates
// is worth: a write shows up in the GitHub request log, but a write that did
// not happen is invisible there precisely because no call was made.
var mCheckRunWrites = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "github_check_run_writes_total",
		Help: "Check run writes by outcome.",
	},
	[]string{"result"}, // created | updated | unchanged
)
