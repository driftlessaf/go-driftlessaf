/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import "errors"

// Credentials selects how a Linear Client authenticates. It carries no
// behaviour beyond NewClient; the env tags name the conventional variables
// so a main package can embed it in its envconfig struct:
//
//	env := envconfig.MustProcess(ctx, &struct {
//		Linear linearreconciler.Credentials
//		// ...
//	}{})
//	client, err := env.Linear.NewClient(linearreconciler.ScopeRead, linearreconciler.ScopeWrite)
//
// Exactly one form must be set: a static API key, or an OAuth
// client-credentials pair. OAuth is preferred for deployments (see NewClient).
type Credentials struct {
	APIKey       string `env:"LINEAR_API_KEY"`
	ClientID     string `env:"LINEAR_CLIENT_ID"`
	ClientSecret string `env:"LINEAR_CLIENT_SECRET"`
}

// NewClient builds a Client from exactly one credential form. Both forms or
// a half-set OAuth pair is a configuration error, so a deployment cannot
// silently run on a personal key while its OAuth secret is also mounted.
// scopes apply to the OAuth form only; empty keeps the Client default.
func (c Credentials) NewClient(scopes ...string) (*Client, error) {
	hasKey := c.APIKey != ""
	hasOAuth := c.ClientID != "" || c.ClientSecret != ""
	switch {
	case hasKey && hasOAuth:
		return nil, errors.New("set LINEAR_API_KEY or LINEAR_CLIENT_ID/LINEAR_CLIENT_SECRET, not both")
	case hasKey:
		return NewClientWithAPIKey(c.APIKey), nil
	case c.ClientID != "" && c.ClientSecret != "":
		client := NewClient(c.ClientID, c.ClientSecret)
		if len(scopes) > 0 {
			client = client.WithScopes(scopes...)
		}
		return client, nil
	case hasOAuth:
		return nil, errors.New("LINEAR_CLIENT_ID and LINEAR_CLIENT_SECRET must be set together")
	default:
		return nil, errors.New("no Linear credentials configured: set LINEAR_API_KEY, or LINEAR_CLIENT_ID and LINEAR_CLIENT_SECRET")
	}
}
