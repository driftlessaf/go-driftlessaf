/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package suspend

import "time"

// SetClock injects a deterministic clock into c, for tests that pin the
// stamped park deadline or advance time past it.
func SetClock(c *Coordinator, now func() time.Time) {
	c.now = now
}
