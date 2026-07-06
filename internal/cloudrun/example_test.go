/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package cloudrun_test

import (
	"cmp"
	"fmt"

	"chainguard.dev/driftlessaf/internal/cloudrun"
)

func ExampleServiceName() {
	// Resolve the running resource's identity, defaulting when unset.
	name := cmp.Or(cloudrun.ServiceName(), "unknown")
	fmt.Println(name)
}

func ExampleRevisionName() {
	revision := cmp.Or(cloudrun.RevisionName(), "unknown")
	fmt.Println(revision)
}

func ExampleIsJob() {
	if cloudrun.IsJob() {
		fmt.Println("running as a Cloud Run job")
	} else {
		fmt.Println("running as a Cloud Run service")
	}
}
