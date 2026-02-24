/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package graphqlclient

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	mGraphQLOperations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "github_graphql_operations_total",
			Help: "Total GitHub GraphQL operations executed.",
		},
		[]string{"operation", "status", "response_code"},
	)

	mGraphQLDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "github_graphql_duration_seconds",
			Help:    "Duration of GitHub GraphQL operations.",
			Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"operation"},
	)
)
