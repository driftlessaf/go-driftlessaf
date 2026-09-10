/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package jitter_test

import (
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/internal/jitter"
)

func ExampleAdd() {
	// A non-positive duration is returned unchanged.
	fmt.Println(jitter.Add(0))
	fmt.Println(jitter.Add(-time.Second) == -time.Second)

	// A positive duration is returned with 0–100% jitter added.
	result := jitter.Add(time.Second)
	fmt.Println(result >= time.Second && result <= 2*time.Second)
	// Output:
	// 0s
	// true
	// true
}
