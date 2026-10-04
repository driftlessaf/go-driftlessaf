/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package serve runs a workqueue.WorkqueueServiceServer on the standard
// duplex gRPC server: trace-parent restoration, OpenTelemetry stats,
// metrics and recovery interceptors, a gRPC health service, and the
// metrics listener. It is the serving half of githubreconciler.Main,
// available to reconcilers that assemble their own server (Linear, OCI,
// APK, or dual-key wrappers) without re-copying the wiring.
//
// Configuration is passed as options. Reading the environment is the
// caller's job, conventionally in a main package via envconfig.
package serve
