/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package modelrouter

import "fmt"

// Provider identifies a serving, authentication, billing, and availability
// boundary. Provider is extensible: applications can use any value accepted by
// Validate without changing this package.
type Provider string

const (
	// ProviderVertexAI serves models through Google Vertex AI.
	ProviderVertexAI Provider = "vertex"
	// ProviderAnthropic serves models through Anthropic's first-party API.
	ProviderAnthropic Provider = "anthropic"
	// ProviderAWSBedrock serves models through Amazon Bedrock.
	ProviderAWSBedrock Provider = "bedrock"
)

// Validate returns an error unless p is a lowercase identifier. Provider
// identifiers may contain ASCII letters, digits, dots, underscores, and
// hyphens, and must start and end with a letter or digit.
func (p Provider) Validate() error {
	value := string(p)
	if value == "" {
		return fmt.Errorf("provider must not be empty")
	}
	for i := range len(value) {
		c := value[i]
		if isLowerAlphaNumeric(c) {
			continue
		}
		if i > 0 && i < len(value)-1 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return fmt.Errorf("invalid provider %q: must be lowercase ASCII letters or digits separated by '.', '_', or '-'", value)
	}
	return nil
}

// Protocol identifies a request and response contract implemented by a
// DriftlessAF executor. Unlike providers, protocols are a controlled set.
type Protocol string

const (
	// ProtocolGoogleGenAI uses the Google Gen AI request and response contract.
	ProtocolGoogleGenAI Protocol = "google-gen-ai"
	// ProtocolAnthropicMessages uses the Anthropic Messages request and
	// response contract.
	ProtocolAnthropicMessages Protocol = "anthropic-messages"
	// ProtocolOpenAIChatCompletions uses the OpenAI Chat Completions request and
	// response contract.
	ProtocolOpenAIChatCompletions Protocol = "openai-chat-completions"
)

// Validate returns an error unless p identifies a protocol implemented by a
// DriftlessAF executor.
func (p Protocol) Validate() error {
	switch p {
	case ProtocolGoogleGenAI, ProtocolAnthropicMessages, ProtocolOpenAIChatCompletions:
		return nil
	default:
		return fmt.Errorf("unsupported protocol %q (want %q, %q, or %q)", p, ProtocolGoogleGenAI, ProtocolAnthropicMessages, ProtocolOpenAIChatCompletions)
	}
}

func isLowerAlphaNumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}
