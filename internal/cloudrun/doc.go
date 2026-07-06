/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package cloudrun resolves the identity of the running Cloud Run resource
// from its environment variables.
//
// Cloud Run services set K_SERVICE and K_REVISION, while Cloud Run jobs set
// CLOUD_RUN_JOB and CLOUD_RUN_EXECUTION instead. These helpers prefer the
// service variables and fall back to the job variables, so a workload reports a
// stable identity whether it runs as a service or a job.
//
// See https://cloud.google.com/run/docs/container-contract#services-env-vars
// and https://cloud.google.com/run/docs/container-contract#jobs-env-vars.
package cloudrun
