/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package modelrouter resolves explicit model-provider selections into
// validated, secret-free route plans.
//
// Applications construct a Registry from their route catalog at startup. A
// route identifies a serving provider, logical model, wire protocol, exact
// provider model ID, provider attribution, and explicit capability allow-set.
// Resolution intersects route, model-family, and protocol support. It does
// not construct provider clients or load credentials; those responsibilities
// belong to protocol adapters.
//
// Providers are extensible validated identifiers. Protocols are controlled
// because each protocol corresponds to executor behavior implemented by
// DriftlessAF.
package modelrouter
