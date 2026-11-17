/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"time"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// WithClock replaces the clock the deadline checks read.
func WithClock[Request promptbuilder.Bindable, Response any](now func() time.Time) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		e.now = now
		return nil
	}
}
