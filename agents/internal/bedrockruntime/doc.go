/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package bedrockruntime supplies the credential boundary for Bedrock Runtime
// protocol adapters. A Client signs requests for one regional HTTPS endpoint
// with the same refreshable provider validated by awsauth.Config.
//
// Clients are safe for concurrent use. They do not follow redirects, use proxy
// environment variables, retry requests, or log request/response payloads. The
// caller must provide a context deadline, close response bodies, and implement
// protocol-level retries. Requests are buffered up to 25 MB for payload signing;
// responses remain streaming. Query strings and non-commercial AWS partitions
// are not supported. Model permissions remain an IAM and adapter concern.
//
// Signed headers stay on a private request. Caller HTTP trace hooks are not run
// inside this boundary because they can observe credential-bearing headers.
// Returned transport errors do not wrap SDK or network errors that may expose
// credentials. Cancellation, deadlines, and network timeout classification are
// preserved. Response payloads are not redacted; protocol consumers must not log
// them or assume they contain no sensitive application data.
package bedrockruntime
