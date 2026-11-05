/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package openaiauth authenticates to OpenAI's first-party API with workload
// identity federation instead of an API key.
//
// The workload presents a Google-issued OIDC ID token, minted through
// Application Default Credentials for the audience OpenAI expects, and the
// openai-go SDK exchanges it at https://auth.openai.com/oauth/token for a
// short-lived bearer token (option.WithWorkloadIdentity). The SDK caches the
// bearer and refreshes it before it expires.
//
// Application Default Credentials cover GKE and Cloud Run workload identity
// (the metadata server), a service account key file, and, for local use, an
// impersonated service account credential file. The SDK's own
// auth.GCPIDTokenProvider is not used because it only talks to the metadata
// server.
//
// The OpenAI organization maps the token's subject (the service account's
// unique ID) to an OpenAI service account through an identity provider
// configured under Organization Settings > Security > Workload Identity.
// Config carries the two opaque IDs that configuration produces; neither is a
// secret.
package openaiauth
