/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/bedrock"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

type anthropicDirectMessagesFactory func(context.Context, anthropicauth.Config) (anthropic.MessageService, error)
type bedrockAWSConfigFactory func(context.Context, awsauth.Config) (aws.Config, error)
type bedrockMessagesFactory func(context.Context, aws.Config, string) (anthropic.MessageService, error)

const bedrockMantleServiceName = "bedrock-mantle"

// NewAnthropicDirectMessagesAdapter constructs an Anthropic-direct adapter
// for the Anthropic Messages protocol. cfg is supplied explicitly and remains
// captured by the adapter; this path doesn't read Anthropic backend-selection
// environment variables or fall back to Vertex AI.
func NewAnthropicDirectMessagesAdapter(cfg anthropicauth.Config) (AnthropicMessagesAdapter, error) {
	return newAnthropicDirectMessagesAdapter(cfg, func(ctx context.Context, cfg anthropicauth.Config) (anthropic.MessageService, error) {
		client, err := anthropicauth.NewDirectClient(ctx, cfg)
		if err != nil {
			return anthropic.MessageService{}, err
		}
		return client.Messages, nil
	})
}

func newAnthropicDirectMessagesAdapter(cfg anthropicauth.Config, newMessages anthropicDirectMessagesFactory) (AnthropicMessagesAdapter, error) {
	if !cfg.Configured() {
		return nil, fmt.Errorf("%w: Anthropic-direct authentication requires a federation rule ID and organization ID", ErrInvalidAdapter)
	}
	if newMessages == nil {
		return nil, fmt.Errorf("%w: Anthropic-direct Messages factory is nil", ErrInvalidAdapter)
	}
	return func(ctx context.Context, plan modelrouter.Plan) (AnthropicMessagesBinding, error) {
		if err := validateAnthropicDirectPlan(plan); err != nil {
			return AnthropicMessagesBinding{}, err
		}
		messages, err := newMessages(ctx, cfg)
		if err != nil {
			return AnthropicMessagesBinding{}, fmt.Errorf("creating Anthropic-direct Messages client: %w", err)
		}
		return NewAnthropicMessagesBinding(plan, messages, nil)
	}, nil
}

// NewBedrockAnthropicMessagesAdapter constructs an AWS Bedrock Mantle adapter
// for the Anthropic Messages protocol. cfg is supplied explicitly and remains
// captured by the adapter; credentials are validated only if a Bedrock route
// selects this adapter.
func NewBedrockAnthropicMessagesAdapter(cfg awsauth.Config) (AnthropicMessagesAdapter, error) {
	return newBedrockAnthropicMessagesAdapter(
		cfg,
		func(ctx context.Context, cfg awsauth.Config) (aws.Config, error) {
			return cfg.LoadAWSConfig(ctx)
		},
		newBedrockMantleMessages,
	)
}

func newBedrockAnthropicMessagesAdapter(
	cfg awsauth.Config,
	loadAWSConfig bedrockAWSConfigFactory,
	newMessages bedrockMessagesFactory,
) (AnthropicMessagesAdapter, error) {
	if strings.TrimSpace(cfg.Region) == "" {
		return nil, fmt.Errorf("%w: Bedrock AWS region cannot be empty", ErrInvalidAdapter)
	}
	if cfg.Region != strings.TrimSpace(cfg.Region) {
		return nil, fmt.Errorf("%w: Bedrock AWS region must not contain leading or trailing whitespace", ErrInvalidAdapter)
	}
	if !isAWSRegion(cfg.Region) {
		return nil, fmt.Errorf("%w: Bedrock AWS region must contain only lowercase letters, digits, or hyphens", ErrInvalidAdapter)
	}
	if cfg.Profile != strings.TrimSpace(cfg.Profile) {
		return nil, fmt.Errorf("%w: Bedrock AWS profile must not contain leading or trailing whitespace", ErrInvalidAdapter)
	}
	if loadAWSConfig == nil {
		return nil, fmt.Errorf("%w: Bedrock AWS config factory is nil", ErrInvalidAdapter)
	}
	if newMessages == nil {
		return nil, fmt.Errorf("%w: Bedrock Messages factory is nil", ErrInvalidAdapter)
	}
	return func(ctx context.Context, plan modelrouter.Plan) (AnthropicMessagesBinding, error) {
		if err := validateBedrockPlan(plan); err != nil {
			return AnthropicMessagesBinding{}, err
		}
		awsConfig, err := loadAWSConfig(ctx, cfg)
		if err != nil {
			return AnthropicMessagesBinding{}, fmt.Errorf("loading Bedrock AWS credentials: %w", err)
		}
		messages, err := newMessages(ctx, awsConfig, bedrockMantleBaseURL(cfg.Region))
		if err != nil {
			return AnthropicMessagesBinding{}, fmt.Errorf("creating Bedrock Mantle Messages client: %w", err)
		}
		return NewAnthropicMessagesBinding(plan, messages, nil)
	}, nil
}

func newBedrockMantleMessages(ctx context.Context, awsConfig aws.Config, baseURL string) (anthropic.MessageService, error) {
	return newBedrockMantleMessagesWithOptions(ctx, awsConfig, baseURL)
}

func newBedrockMantleMessagesWithOptions(
	ctx context.Context,
	awsConfig aws.Config,
	baseURL string,
	opts ...option.RequestOption,
) (anthropic.MessageService, error) {
	opts = append([]option.RequestOption{option.WithoutEnvironmentDefaults()}, opts...)
	opts = append(opts, option.WithMiddleware(bedrockMantleSigV4Middleware(awsConfig)))
	client, err := bedrock.NewMantleClient(ctx, bedrock.MantleClientConfig{
		AWSRegion: awsConfig.Region,
		BaseURL:   baseURL,
		SkipAuth:  true,
	}, opts...)
	if err != nil {
		return anthropic.MessageService{}, err
	}
	return client.Messages, nil
}

func bedrockMantleSigV4Middleware(awsConfig aws.Config) option.Middleware {
	signer := v4.NewSigner()
	return func(request *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		var body []byte
		if request.Body != nil {
			var err error
			body, err = io.ReadAll(request.Body)
			if err != nil {
				return nil, fmt.Errorf("reading Bedrock request body: %w", err)
			}
			if err := request.Body.Close(); err != nil {
				return nil, fmt.Errorf("closing Bedrock request body: %w", err)
			}
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		request.ContentLength = int64(len(body))

		credentials, err := awsConfig.Credentials.Retrieve(request.Context())
		if err != nil {
			return nil, fmt.Errorf("retrieving Bedrock AWS credentials: %w", err)
		}
		hash := sha256.Sum256(body)
		if err := signer.SignHTTP(
			request.Context(),
			credentials,
			request,
			hex.EncodeToString(hash[:]),
			bedrockMantleServiceName,
			awsConfig.Region,
			time.Now(),
		); err != nil {
			return nil, fmt.Errorf("signing Bedrock request: %w", err)
		}
		return next(request)
	}
}

func bedrockMantleBaseURL(region string) string {
	return fmt.Sprintf("https://bedrock-mantle.%s.api.aws/anthropic", region)
}

func isAWSRegion(region string) bool {
	for i := range len(region) {
		c := region[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		if c == '-' && i > 0 && i < len(region)-1 {
			continue
		}
		return false
	}
	return true
}

func validateAnthropicDirectPlan(plan modelrouter.Plan) error {
	return validateClaudeProviderPlan(
		plan,
		modelrouter.ProviderAnthropic,
		agenttrace.SystemAnthropic,
		agenttrace.SystemAnthropic,
	)
}

func validateBedrockPlan(plan modelrouter.Plan) error {
	return validateClaudeProviderPlan(
		plan,
		modelrouter.ProviderAWSBedrock,
		agenttrace.SystemBedrock,
		agenttrace.SystemBedrock,
	)
}

func validateClaudeProviderPlan(
	plan modelrouter.Plan,
	provider modelrouter.Provider,
	providerName, legacySystem string,
) error {
	if err := validateBindingPlan(plan, modelrouter.ProtocolAnthropicMessages); err != nil {
		return err
	}
	if plan.Provider() != provider {
		return fmt.Errorf("%w: %s adapter received provider %q", ErrInvalidBinding, provider, plan.Provider())
	}
	attribution := plan.Attribution()
	if attribution.ProviderName != providerName || attribution.LegacySystem != legacySystem {
		return fmt.Errorf("%w: %s route attribution must use provider name %q and legacy system %q, got %q and %q",
			ErrInvalidBinding, provider, providerName, legacySystem, attribution.ProviderName, attribution.LegacySystem)
	}
	return nil
}
