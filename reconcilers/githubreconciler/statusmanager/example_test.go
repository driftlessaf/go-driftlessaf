/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
)

// ExampleStatus demonstrates constructing a Status value with custom details.
func ExampleStatus() {
	type MyDetails struct {
		FilesProcessed int
	}

	s := statusmanager.Status[MyDetails]{
		ObservedGeneration: "abc123",
		Status:             "completed",
		Conclusion:         "success",
		Details:            MyDetails{FilesProcessed: 5},
	}
	fmt.Printf("generation=%s status=%s conclusion=%s files=%d\n",
		s.ObservedGeneration, s.Status, s.Conclusion, s.Details.FilesProcessed)
	// Output: generation=abc123 status=completed conclusion=success files=5
}

func ExampleSession_CheckRunStatus() {
	var session statusmanager.Session[struct{}]
	status, conclusion := session.CheckRunStatus()
	fmt.Printf("status=%q conclusion=%q\n", status, conclusion)
	// Output: status="" conclusion=""
}
