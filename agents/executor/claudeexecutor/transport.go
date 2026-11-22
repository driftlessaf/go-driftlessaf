/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"errors"

	"chainguard.dev/driftlessaf/agents/executor/internal/execshared"
	"github.com/anthropics/anthropic-sdk-go"
)

// isTransportDrop reports whether err is the transport dropping a response
// stream mid-flight; see execshared.IsTransportDrop for the shapes. The
// turn's message is appended only after the stream completes, so re-sending
// the same request duplicates nothing.
func isTransportDrop(err error) bool {
	return execshared.IsTransportDrop(err, isAPIError)
}

// isAPIError reports whether err carries an *anthropic.Error. An API error
// carries a verdict, so it is never a transport drop. It is also the one
// error whose Error() needs its request populated.
func isAPIError(err error) bool {
	_, ok := errors.AsType[*anthropic.Error](err)
	return ok
}
