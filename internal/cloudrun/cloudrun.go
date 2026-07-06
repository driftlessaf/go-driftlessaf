/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package cloudrun

import "os"

// ServiceName returns the identifier of the running Cloud Run resource,
// preferring K_SERVICE (services) and falling back to CLOUD_RUN_JOB (jobs).
// It returns "" if neither is set.
func ServiceName() string {
	if s := os.Getenv("K_SERVICE"); s != "" {
		return s
	}
	return os.Getenv("CLOUD_RUN_JOB")
}

// RevisionName returns the revision/execution identifier of the running Cloud
// Run resource, preferring K_REVISION (services) and falling back to
// CLOUD_RUN_EXECUTION (jobs). It returns "" if neither is set.
func RevisionName() string {
	if r := os.Getenv("K_REVISION"); r != "" {
		return r
	}
	return os.Getenv("CLOUD_RUN_EXECUTION")
}

// IsJob reports whether the process is running as a Cloud Run job.
func IsJob() bool {
	return os.Getenv("CLOUD_RUN_JOB") != ""
}
