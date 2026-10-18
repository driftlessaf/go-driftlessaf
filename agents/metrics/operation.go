/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics

import "context"

// defaultOperationName is the gen_ai.operation.name recorded when the context
// carries none: an agent invocation.
const defaultOperationName = "invoke_agent"

type operationNameKey struct{}

// WithOperationName returns a context whose token recordings carry name as
// gen_ai.operation.name instead of "invoke_agent", for example
// "batch_predict" for batch prediction. An empty name keeps the default.
func WithOperationName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, operationNameKey{}, name)
}

// operationName returns the gen_ai.operation.name carried on ctx, or
// "invoke_agent" when none was set.
func operationName(ctx context.Context) string {
	if name, _ := ctx.Value(operationNameKey{}).(string); name != "" {
		return name
	}
	return defaultOperationName
}
