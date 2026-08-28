/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs_test

import (
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/workqueue/gcs"
)

// ExampleNewWorkQueue demonstrates constructing a GCS-backed workqueue.
func ExampleNewWorkQueue() {
	// In production, pass a real *storage.BucketHandle obtained from a
	// cloud.google.com/go/storage client.
	//
	//   client, err := storage.NewClient(ctx)
	//   bucket := client.Bucket("my-workqueue-bucket")
	//   wq := gcs.NewWorkQueue(bucket, 10)
	//
	// The limit parameter controls the maximum number of keys dequeued per
	// Enumerate call.
	fmt.Println("GCS workqueue limit:", 10)
	// Output: GCS workqueue limit: 10
}

// ExampleRefreshInterval demonstrates that the lease refresh interval is
// configurable for testing or custom deployments.
func ExampleRefreshInterval() {
	fmt.Println("Default refresh interval:", gcs.RefreshInterval)
	// Output: Default refresh interval: 5m0s
}

// ExampleWithScheduledWaitWarningThreshold demonstrates enabling the optional
// structured warning for keys that wait too long after becoming eligible.
func ExampleWithScheduledWaitWarningThreshold() {
	const threshold = 30 * time.Minute
	_ = gcs.NewWorkQueue(nil, 10, gcs.WithScheduledWaitWarningThreshold(threshold))

	fmt.Println("Scheduled wait warning threshold:", threshold)
	// Output: Scheduled wait warning threshold: 30m0s
}
