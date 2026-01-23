/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
)

// AttributeEnricher enriches metric attributes with additional context.
// This allows different agents to add their own contextual attributes
// without coupling executors to specific use cases (e.g., PR tracking, package versions).
// The enricher receives base attributes (model, tool) and returns an enriched set.
type AttributeEnricher func(ctx context.Context, baseAttrs []attribute.KeyValue) []attribute.KeyValue
