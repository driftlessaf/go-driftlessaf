/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudebackend

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"github.com/anthropics/anthropic-sdk-go"
)

// Resolved contains the provider-specific inputs for a Claude executor.
type Resolved struct {
	Messages anthropic.MessageService
	ModelID  string
	Provider claudeexecutor.Provider
}

// Resolve selects the configured Claude backend and normalizes its model ID.
func Resolve(ctx context.Context, projectID, region, model string) (Resolved, error) {
	authCfg, err := anthropicauth.ConfigFromEnv()
	if err != nil {
		return Resolved{}, fmt.Errorf("resolving anthropic auth config: %w", err)
	}
	return resolve(ctx, projectID, region, model, authCfg), nil
}

func resolve(ctx context.Context, projectID, region, model string, authCfg anthropicauth.Config) Resolved {
	resolved := Resolved{
		Messages: anthropicauth.NewClient(ctx, projectID, region, authCfg).Messages,
		ModelID:  model,
		Provider: claudeexecutor.ProviderVertex,
	}
	if authCfg.Configured() {
		resolved.ModelID = anthropicauth.ModelID(model)
		resolved.Provider = claudeexecutor.ProviderAnthropic
	}
	return resolved
}
