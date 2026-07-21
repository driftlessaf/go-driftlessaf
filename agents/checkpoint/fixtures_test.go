/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package checkpoint_test

import (
	"encoding/json"
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/memstore"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"google.golang.org/genai"
)

// roundTripThroughEnvelope marshals a real provider payload, parks it in an
// Envelope.ProviderState, pushes it through a Store, and asserts (a) the raw
// bytes survive the Store byte-for-byte and (b) the payload re-marshals
// byte-identically after a full unmarshal into the SDK type — i.e. the envelope
// carries a real, resumable SDK payload, not a lossy projection. reparse
// decodes the raw back into the concrete SDK type and re-encodes it.
func roundTripThroughEnvelope(t *testing.T, provider string, payload any, reparse func(json.RawMessage) (json.RawMessage, error)) {
	t.Helper()

	rawIn, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", provider, err)
	}

	env := &checkpoint.Envelope{
		Version:       checkpoint.EnvelopeVersion,
		Provider:      provider,
		Model:         "fixture-model",
		ProviderState: json.RawMessage(rawIn),
	}

	store := memstore.New()
	if err := store.Save(t.Context(), "k", env); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _, ok, err := store.Load(t.Context(), "k")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}

	// (a) The raw provider payload survives the store byte-for-byte.
	if string(got.ProviderState) != string(rawIn) {
		t.Fatalf("%s ProviderState not byte-identical through store:\n want=%s\n  got=%s",
			provider, rawIn, got.ProviderState)
	}

	// (b) The payload is a real SDK payload: decoding into the concrete type and
	// re-encoding reproduces the same bytes.
	reparsed, err := reparse(got.ProviderState)
	if err != nil {
		t.Fatalf("reparse %s payload: %v", provider, err)
	}
	if string(reparsed) != string(rawIn) {
		t.Fatalf("%s payload not byte-identical after SDK round-trip:\n want=%s\n  got=%s",
			provider, rawIn, reparsed)
	}
}

func TestEnvelopeCarriesAnthropicPayload(t *testing.T) {
	// A realistic assistant turn: a thinking block with a signature, a text
	// block carrying a cache_control breakpoint, and a tool_use block with a
	// raw-JSON input — the exact shapes a suspended Claude conversation holds.
	params := anthropic.MessageNewParams{
		Model:     "claude-fable-5",
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{{
			Role: anthropic.MessageParamRoleAssistant,
			Content: []anthropic.ContentBlockParamUnion{
				{OfThinking: &anthropic.ThinkingBlockParam{
					Thinking:  "The user asked whether to proceed; I should ask a human.",
					Signature: "c2lnbmF0dXJlLWJ5dGVz",
				}},
				{OfText: &anthropic.TextBlockParam{
					Text:         "Let me confirm with a human before continuing.",
					CacheControl: anthropic.NewCacheControlEphemeralParam(),
				}},
				{OfToolUse: &anthropic.ToolUseBlockParam{
					ID:    "toolu_01ABCDEF",
					Name:  "ask_a_friend",
					Input: json.RawMessage(`{"question":"Should I force-push?"}`),
				}},
			},
		}},
	}
	roundTripThroughEnvelope(t, "anthropic", params, func(raw json.RawMessage) (json.RawMessage, error) {
		var p anthropic.MessageNewParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return json.Marshal(p)
	})
}

func TestEnvelopeCarriesGenaiHistory(t *testing.T) {
	// A genai model turn carrying a thoughtSignature ([]byte, base64 on the
	// wire) plus a function call with the persisted call ID.
	history := []*genai.Content{
		{
			Role:  "user",
			Parts: []*genai.Part{{Text: "Should I force-push?"}},
		},
		{
			Role: "model",
			Parts: []*genai.Part{
				{
					Text:             "I need to check with a human.",
					Thought:          true,
					ThoughtSignature: []byte("thought-signature-bytes"),
				},
				{FunctionCall: &genai.FunctionCall{
					ID:   "fc_01",
					Name: "ask_a_friend",
					Args: map[string]any{"question": "Should I force-push?"},
				}},
			},
		},
	}
	roundTripThroughEnvelope(t, "google", history, func(raw json.RawMessage) (json.RawMessage, error) {
		var h []*genai.Content
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, err
		}
		return json.Marshal(h)
	})
}

func TestEnvelopeCarriesOpenAIPayload(t *testing.T) {
	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModelGPT4o,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Should I force-push?"),
			openai.ToolMessage(`{"answer":"pending"}`, "call_01"),
		},
	}
	roundTripThroughEnvelope(t, "openai", params, func(raw json.RawMessage) (json.RawMessage, error) {
		var p openai.ChatCompletionNewParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return json.Marshal(p)
	})
}
