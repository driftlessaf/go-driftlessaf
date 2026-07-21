/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package effort_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/effort"
)

func ExampleLevel_Validate() {
	if err := effort.XHigh.Validate(); err != nil {
		fmt.Println("unexpected:", err)
	}

	if err := effort.Level("extreme").Validate(); err != nil {
		fmt.Println(err)
	}
	// Output:
	// invalid effort level "extreme" (want low|medium|high|xhigh|max)
}
