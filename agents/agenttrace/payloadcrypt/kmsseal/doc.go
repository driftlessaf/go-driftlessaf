/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package kmsseal provides the Cloud KMS-backed wrap for payloadcrypt. It is
// deliberately separate from payloadcrypt so the core sealing package (imported
// transitively by every agenttrace consumer) stays free of
// cloud.google.com/go/kms — only code that actually seals payloads in production
// (the vuln-patcher processors) imports this package and pulls in the KMS SDK.
package kmsseal
