/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"time"

	"chainguard.dev/driftlessaf/agents/executor/internal/execshared"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// http2StreamError lets the transport tests check that the shared mirror of
// net/http's HTTP/2 stream error still matches the real one.
type http2StreamError = execshared.HTTP2StreamError

// WithClock replaces the clock the deadline checks read.
func WithClock[Request promptbuilder.Bindable, Response any](now func() time.Time) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		e.now = now
		return nil
	}
}
