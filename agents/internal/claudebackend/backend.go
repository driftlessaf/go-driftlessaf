/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudebackend

import (
	"context"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/bedrock"
	"github.com/sethvargo/go-envconfig"
)

type backend string

const (
	backendVertex    backend = "vertex"
	backendAnthropic backend = "anthropic"
	backendBedrock   backend = "bedrock"

	envBackend = "CLAUDE_BACKEND"
)

type environment struct {
	Backend          string `env:"CLAUDE_BACKEND"`
	AnthropicProfile string `env:"ANTHROPIC_PROFILE"`
}

type config struct {
	backend   backend
	anthropic anthropicauth.Config
	aws       awsauth.Config
}

type mantleMessagesFactory func(context.Context, awsauth.Config) (anthropic.MessageService, error)

// Resolved contains the provider-specific inputs for a Claude executor.
type Resolved struct {
	Messages anthropic.MessageService
	ModelID  string
	Provider claudeexecutor.Provider
}

// Resolve selects the configured Claude backend and normalizes its model ID.
func Resolve(ctx context.Context, projectID, region, model string) (Resolved, error) {
	cfg, err := configFromEnv(ctx)
	if err != nil {
		return Resolved{}, err
	}
	return resolve(ctx, projectID, region, model, cfg, newMantleMessages)
}

func configFromEnv(ctx context.Context) (config, error) {
	var env environment
	if err := envconfig.Process(ctx, &env); err != nil {
		return config{}, fmt.Errorf("reading Claude backend environment: %w", err)
	}

	switch selected := backend(env.Backend); selected {
	case "":
		authCfg, err := anthropicauth.ConfigFromEnv()
		if err != nil {
			return config{}, fmt.Errorf("resolving anthropic auth config: %w", err)
		}
		selected = backendVertex
		if authCfg.Configured() {
			selected = backendAnthropic
		}
		return config{backend: selected, anthropic: authCfg}, nil
	case backendVertex:
		if env.AnthropicProfile != "" {
			return config{}, fmt.Errorf("%s=%q conflicts with %s", envBackend, selected, anthropicauth.EnvProfile)
		}
		return config{backend: selected}, nil
	case backendAnthropic:
		if env.AnthropicProfile == "" {
			return config{}, fmt.Errorf("%s=%q requires %s", envBackend, selected, anthropicauth.EnvProfile)
		}
		authCfg, err := anthropicauth.ConfigFromEnv()
		if err != nil {
			return config{}, fmt.Errorf("resolving anthropic auth config: %w", err)
		}
		return config{backend: selected, anthropic: authCfg}, nil
	case backendBedrock:
		if env.AnthropicProfile != "" {
			return config{}, fmt.Errorf("%s=%q conflicts with %s", envBackend, selected, anthropicauth.EnvProfile)
		}
		authCfg, err := awsauth.ConfigFromEnv(ctx)
		if err != nil {
			return config{}, fmt.Errorf("resolving AWS auth config: %w", err)
		}
		return config{backend: selected, aws: authCfg}, nil
	default:
		return config{}, fmt.Errorf("unknown %s value %q; want %q, %q, or %q", envBackend, env.Backend, backendVertex, backendAnthropic, backendBedrock)
	}
}

func resolve(ctx context.Context, projectID, region, model string, cfg config, newMantle mantleMessagesFactory) (Resolved, error) {
	switch cfg.backend {
	case backendVertex:
		return Resolved{
			Messages: anthropicauth.NewClient(ctx, projectID, region, anthropicauth.Config{}).Messages,
			ModelID:  model,
			Provider: claudeexecutor.ProviderVertex,
		}, nil
	case backendAnthropic:
		return Resolved{
			Messages: anthropicauth.NewClient(ctx, projectID, region, cfg.anthropic).Messages,
			ModelID:  anthropicauth.ModelID(model),
			Provider: claudeexecutor.ProviderAnthropic,
		}, nil
	case backendBedrock:
		modelID, err := mantleModelID(model)
		if err != nil {
			return Resolved{}, err
		}
		messages, err := newMantle(ctx, cfg.aws)
		if err != nil {
			return Resolved{}, fmt.Errorf("constructing Bedrock Mantle client: %w", err)
		}
		return Resolved{
			Messages: messages,
			ModelID:  modelID,
			Provider: claudeexecutor.ProviderBedrock,
		}, nil
	default:
		return Resolved{}, fmt.Errorf("unsupported Claude backend %q", cfg.backend)
	}
}

func mantleModelID(model string) (string, error) {
	if strings.Contains(model, "@") {
		return "", fmt.Errorf("model %q for Bedrock Mantle must not contain a Vertex @version suffix", model)
	}
	if strings.HasPrefix(model, "anthropic.claude-") && model != "anthropic.claude-" {
		return model, nil
	}
	if !strings.HasPrefix(model, "claude-") || model == "claude-" {
		return "", fmt.Errorf("model %q for Bedrock Mantle must use the claude-* or anthropic.claude-* format", model)
	}
	return "anthropic." + model, nil
}

func newMantleMessages(ctx context.Context, cfg awsauth.Config) (anthropic.MessageService, error) {
	if err := cfg.ValidateCredentials(ctx); err != nil {
		return anthropic.MessageService{}, err
	}
	client, err := bedrock.NewMantleClient(ctx, bedrock.MantleClientConfig{
		AWSProfile: cfg.Profile,
		AWSRegion:  cfg.Region,
	})
	if err != nil {
		return anthropic.MessageService{}, err
	}
	return client.Messages, nil
}
