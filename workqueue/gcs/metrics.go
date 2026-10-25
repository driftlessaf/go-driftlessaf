/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"chainguard.dev/driftlessaf/internal/cloudrun"
)

// baseServiceName and baseRevisionName label every workqueue metric with the
// running Cloud Run resource's identity. They prefer the service variables
// (K_SERVICE/K_REVISION) and fall back to the job variables
// (CLOUD_RUN_JOB/CLOUD_RUN_EXECUTION), defaulting to "unknown".
var (
	baseServiceName  = cmp.Or(cloudrun.ServiceName(), "unknown")
	baseRevisionName = cmp.Or(cloudrun.RevisionName(), "unknown")
)

// queue_name labels every workqueue metric so consumers can distinguish
// multiple workqueues running inside the same Cloud Run service. It is set
// via gcs.WithName when constructing the queue and defaults to the empty
// string for queues that don't pass the option (preserves existing query
// shapes — empty is just an additional dimension).
var (
	// TODO(mattmoor): Inspiration:
	// https://pkg.go.dev/k8s.io/client-go/util/workqueue#MetricsProvider

	mInProgressKeys = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workqueue_in_progress_keys",
			Help: "The number of keys currently being processed by this workqueue.",
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mInProgressKeysByOwner = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workqueue_in_progress_keys_by_owner",
			Help: "The number of in-progress keys grouped by the owner recorded when each key was started (e.g. the claiming dispatcher's region). Keys without a recorded owner count as \"unknown\".",
		},
		[]string{"service_name", "revision_name", "queue_name", "owner"},
	)
	mQueuedKeys = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workqueue_queued_keys",
			Help: "The number of keys currently in the backlog of this workqueue.",
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mQueuedKeysByTenant = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workqueue_queued_keys_by_tenant",
			Help: "The number of eligible queued keys per tenant, for the tenants with the largest backlog; the rest are summed into one series with overflow=\"true\" and an empty tenant. Emitted only by queues configured WithTenant.",
		},
		[]string{"service_name", "revision_name", "queue_name", "tenant", "overflow"},
	)
	mQueuedKeysLowerBound = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workqueue_queued_keys_lower_bound",
			Help: "The number of queued objects refreshed on every capacity-aware pass; exact when capacity is available and a bounded lower bound while dispatch is at capacity.",
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mNotBeforeKeys = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workqueue_notbefore_keys",
			Help: "The number of keys waiting on a 'not before' in the backlog of this workqueue.",
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mMaxAttempts = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workqueue_max_attempts",
			Help: "The maximum number of attempts for any queued or in-progress task.",
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mWorkLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "workqueue_process_latency_seconds",
			Help:    "The duration taken to process a key.",
			Buckets: []float64{.25, .5, 1, 2.5, 5, 10, 20, 30, 45, 60, 120, 240, 480, 960, 3600 /* 1h */, 7200 /* 2h */, 14400 /* 4h */, 28800 /* 8h */, 43200 /* 12h */, 86400 /* 24h */},
		},
		[]string{"service_name", "revision_name", "queue_name", "priority_class"},
	)
	mWaitLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "workqueue_wait_latency_seconds",
			Help:    "The duration the key waited to start.",
			Buckets: []float64{.25, .5, 1, 2.5, 5, 10, 20, 30, 45, 60, 120, 240, 480, 960, 3600 /* 1h */, 7200 /* 2h */, 14400 /* 4h */, 28800 /* 8h */, 43200 /* 12h */, 86400 /* 24h */},
		},
		[]string{"service_name", "revision_name", "queue_name", "priority_class"},
	)
	mWaitLatencyFromScheduled = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "workqueue_wait_latency_from_scheduled_seconds",
			Help: "The duration the key waited to start from its scheduled time.",
			Buckets: append(
				append(
					[]float64{.25, .5, 1, 2.5, 5, 10, 20, 30, 45, 60, 120, 240, 480, 960},
					prometheus.LinearBuckets(3600, 3600, 12 /* 1h through 12h */)...,
				),
				86400, // 24h
			),
		},
		[]string{"service_name", "revision_name", "queue_name", "priority_class"},
	)
	mAddedKeys = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workqueue_added_keys",
			Help: "The total number of queue requests.",
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mDedupedKeys = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workqueue_deduped_keys",
			Help: "The total number of keys that were deduped.",
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mCompletionAttempts = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "workqueue_attempts_at_completion",
			Help:    "The number of attempts for successfully completed tasks",
			Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024},
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mLostStartRaces = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workqueue_lost_start_races_total",
			Help: "The number of Start calls that failed because another dispatcher claimed the key first.",
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mDeadLetteredKeys = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workqueue_dead_lettered_keys",
			Help: "The number of keys currently in the dead letter queue",
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mTimeToCompletion = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "workqueue_time_to_completion_seconds",
			Help:    "The time from first queue to final outcome (success or dead-letter). The metric captures the full lifecycle duration including all retry attempts and backoff delays.",
			Buckets: []float64{5, 10, 20, 30, 45, 60, 120, 240, 480, 960, 3600 /* 1h */, 7200 /* 2h */, 14400 /* 4h */, 28800 /* 8h */, 43200 /* 12h */, 86400 /* 1d */, 172800 /* 2d */, 259200 /* 3d */},
		},
		[]string{"service_name", "revision_name", "queue_name", "priority_class", "status"},
	)
	mLeaseAge = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "workqueue_lease_age_seconds",
			Help:    "The age of active (non-expired) leases for in-progress keys. Measured as time since the key moved to in-progress state.",
			Buckets: []float64{30, 60, 120, 180, 240, 300 /* 5min */, 600 /* 10min */, 900 /* 15min */, 1200 /* 20min */, 1800 /* 30min */, 3600 /* 1h */, 7200 /* 2h */, 14400 /* 4h */, 28800 /* 8h */, 43200 /* 12h */, 86400 /* 24h */},
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mExpiredLeases = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workqueue_expired_leases_total",
			Help: "The total number of lease expiries observed on in-progress keys that a Requeue or Deadletter call then attempted to move.",
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mTimeUntilEligible = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "workqueue_time_until_eligible_seconds",
			Help:    "The time remaining until a queued key becomes eligible to be picked up (based on not-before timestamp). Zero or negative values indicate immediately eligible keys.",
			Buckets: []float64{0, 30, 60, 120, 300 /* 5min */, 600 /* 10min */, 1800 /* 30min */, 3600 /* 1h */, 7200 /* 2h */, 14400 /* 4h */, 28800 /* 8h */, 43200 /* 12h */, 86400 /* 1d */, 172800 /* 2d */, 259200 /* 3d */, 345600 /* 4d */, 432000 /* 5d */, 518400 /* 6d */, 604800 /* 7d */},
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mEnumerateLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "workqueue_enumerate_latency_seconds",
			Help:    "The duration of Enumerate() calls to list and process GCS objects.",
			Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10, 20, 30, 45, 60},
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	// QueuedDepth is polled rather than called once per pass, so it gets its own
	// pair. Without them a stalled producer looks the same whether the queue is
	// full and it is throttling correctly, or the depth read is failing and the
	// caller is swallowing it.
	mQueuedDepthLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "workqueue_queued_depth_latency_seconds",
			Help:    "The duration of QueuedDepth() calls counting the keys under queued/.",
			Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mQueuedDepthErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workqueue_queued_depth_errors_total",
			Help: "The number of QueuedDepth() calls that failed to read the queue's depth.",
		},
		[]string{"service_name", "revision_name", "queue_name"},
	)
	mCapacityAwareEnumerationErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workqueue_capacity_aware_enumeration_errors_total",
			Help: "The number of bounded capacity-aware enumeration reads that failed while dispatch was at capacity.",
		},
		[]string{"service_name", "revision_name", "queue_name", "operation"},
	)
)

// topTenants is how many tenants workqueue_queued_keys_by_tenant names; the
// rest share the overflow series, which bounds the label's cardinality. The
// overflow series is told apart by its own label rather than a reserved
// tenant name, because a TenantFunc may return any string.
const topTenants = 10

// setQueuedKeysByTenant replaces the workqueue_queued_keys_by_tenant series
// for labels with the topTenants largest backlogs, plus the overflow series.
func setQueuedKeysByTenant(labels prometheus.Labels, backlog map[string]int) {
	tenants := slices.Collect(maps.Keys(backlog))
	slices.SortFunc(tenants, func(a, b string) int {
		return cmp.Or(cmp.Compare(backlog[b], backlog[a]), cmp.Compare(a, b))
	})
	mQueuedKeysByTenant.DeletePartialMatch(labels)
	other := 0
	for i, tenant := range tenants {
		if i >= topTenants {
			other += backlog[tenant]
			continue
		}
		l := maps.Clone(labels)
		l["tenant"] = tenant
		l["overflow"] = "false"
		mQueuedKeysByTenant.With(l).Set(float64(backlog[tenant]))
	}
	if len(tenants) > topTenants {
		l := maps.Clone(labels)
		l["tenant"] = ""
		l["overflow"] = "true"
		mQueuedKeysByTenant.With(l).Set(float64(other))
	}
}

// depthLabels are the base labels for a QueuedDepth call. It mirrors
// wq.baseLabels, which QueuedDepth cannot use: it is a free function over a
// bucket rather than a method on a queue, so the queue's name is a parameter.
func depthLabels(queueName string) prometheus.Labels {
	return prometheus.Labels{
		"service_name":  baseServiceName,
		"revision_name": baseRevisionName,
		"queue_name":    queueName,
	}
}

// priorityClass converts a priority value to a priority class label.
func priorityClass(priority int64) string {
	return fmt.Sprintf("%dxx", priority/100)
}
