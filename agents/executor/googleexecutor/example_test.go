/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"google.golang.org/genai"
)

// MathRequest is a sample request type for math problems
type MathRequest struct {
	Problem string
}

// Bind implements promptbuilder.Bindable
func (r *MathRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p.BindXML("problem", struct {
		XMLName struct{} `xml:"problem"`
		Content string   `xml:",chardata"`
	}{
		Content: r.Problem,
	})
}

// MathResponse is a sample response type for math solutions
type MathResponse struct {
	Answer    json.Number `json:"answer"`
	Reasoning string      `json:"reasoning"`
}

// Example demonstrates basic usage of the Google AI executor
func Example() {
	ctx := context.Background()

	// Create Gemini client
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  "my-project",
		Location: "us-central1",
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	// Create prompt template
	prompt, err := promptbuilder.NewPrompt(`You are a math assistant.

Problem: {{problem}}

Solve this and respond in JSON format:
{
  "answer": "the numerical answer",
  "reasoning": "brief explanation"
}`)
	if err != nil {
		log.Fatalf("Failed to create prompt: %v", err)
	}

	// Create executor with default settings
	executor, err := googleexecutor.New[*MathRequest, *MathResponse](
		client,
		prompt,
	)
	if err != nil {
		log.Fatalf("Failed to create executor: %v", err)
	}

	// Execute a request
	request := &MathRequest{Problem: "What is 15 + 27?"}
	response, err := executor.Execute(ctx, request, nil)
	if err != nil {
		log.Fatalf("Execute failed: %v", err)
	}

	fmt.Printf("Answer: %s\n", response.Answer)
}

// Example_withOptions demonstrates using configuration options
func Example_withOptions() {
	ctx := context.Background()

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  "my-project",
		Location: "us-central1",
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	prompt, err := promptbuilder.NewPrompt(`Solve: {{problem}}`)
	if err != nil {
		log.Fatalf("Failed to create prompt: %v", err)
	}

	// Create executor with custom options
	executor, err := googleexecutor.New[*MathRequest, *MathResponse](
		client,
		prompt,
		googleexecutor.WithModel[*MathRequest, *MathResponse]("gemini-2.5-flash"),
		googleexecutor.WithTemperature[*MathRequest, *MathResponse](0.1),
		googleexecutor.WithMaxOutputTokens[*MathRequest, *MathResponse](4096),
		googleexecutor.WithResponseMIMEType[*MathRequest, *MathResponse]("application/json"),
	)
	if err != nil {
		log.Fatalf("Failed to create executor: %v", err)
	}

	request := &MathRequest{Problem: "What is 42 * 13?"}
	response, err := executor.Execute(ctx, request, nil)
	if err != nil {
		log.Fatalf("Execute failed: %v", err)
	}

	fmt.Printf("Answer: %s\n", response.Answer)
}

// Example_withThinking demonstrates enabling thinking mode
func Example_withThinking() {
	ctx := context.Background()

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  "my-project",
		Location: "us-central1",
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	prompt, err := promptbuilder.NewPrompt(`Solve this complex problem: {{problem}}`)
	if err != nil {
		log.Fatalf("Failed to create prompt: %v", err)
	}

	// Enable thinking mode with a 2048 token budget
	executor, err := googleexecutor.New[*MathRequest, *MathResponse](
		client,
		prompt,
		googleexecutor.WithModel[*MathRequest, *MathResponse]("gemini-2.5-flash"),
		googleexecutor.WithMaxOutputTokens[*MathRequest, *MathResponse](8192),
		googleexecutor.WithThinking[*MathRequest, *MathResponse](2048),
		googleexecutor.WithResponseMIMEType[*MathRequest, *MathResponse]("application/json"),
	)
	if err != nil {
		log.Fatalf("Failed to create executor: %v", err)
	}

	request := &MathRequest{Problem: "What is the square root of 144?"}
	response, err := executor.Execute(ctx, request, nil)
	if err != nil {
		log.Fatalf("Execute failed: %v", err)
	}

	fmt.Printf("Answer: %s\n", response.Answer)
}

// Example_withSystemInstructions demonstrates using system instructions
func Example_withSystemInstructions() {
	ctx := context.Background()

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  "my-project",
		Location: "us-central1",
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	// Create system instructions
	systemPrompt, err := promptbuilder.NewPrompt(`You are an expert mathematician.
Always show your work step by step.
Provide clear, concise explanations.`)
	if err != nil {
		log.Fatalf("Failed to create system prompt: %v", err)
	}

	prompt, err := promptbuilder.NewPrompt(`Problem: {{problem}}`)
	if err != nil {
		log.Fatalf("Failed to create prompt: %v", err)
	}

	// Create executor with system instructions
	executor, err := googleexecutor.New[*MathRequest, *MathResponse](
		client,
		prompt,
		googleexecutor.WithSystemInstructions[*MathRequest, *MathResponse](systemPrompt),
		googleexecutor.WithResponseMIMEType[*MathRequest, *MathResponse]("application/json"),
	)
	if err != nil {
		log.Fatalf("Failed to create executor: %v", err)
	}

	request := &MathRequest{Problem: "What is 25% of 80?"}
	response, err := executor.Execute(ctx, request, nil)
	if err != nil {
		log.Fatalf("Execute failed: %v", err)
	}

	fmt.Printf("Answer: %s\n", response.Answer)
}
