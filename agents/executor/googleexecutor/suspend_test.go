/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	agentexecutor "chainguard.dev/driftlessaf/agents/executor"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/genai"
)

// parkTurnJSON is a thinking model's turn that calls lookup twice with the
// ask tool between them. The calls carry no IDs, as Vertex AI often sends
// them, and the thought and the first call carry thought signatures that a
// resumed request must replay unchanged.
const parkTurnJSON = `{
	"candidates":[{"content":{"role":"model","parts":[
		{"text":"I need the operator.","thought":true,"thoughtSignature":"c2lnLXRob3VnaHQ="},
		{"functionCall":{"name":"lookup","args":{"q":"a"}},"thoughtSignature":"c2lnLWNhbGw="},
		{"functionCall":{"name":"ask","args":{"question":"which version?","context":"both build"}}},
		{"functionCall":{"name":"lookup","args":{"q":"b"}}}
	]}}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
}`

// askAndSubmitTurnJSON calls the ask tool and submit_result in one turn.
const askAndSubmitTurnJSON = `{
	"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"id":"call_ask","name":"ask","args":{"question":"moot?"}}},
		{"functionCall":{"id":"call_submit","name":"submit_result","args":{"answer":"42"}}}
	]}}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
}`

// recordedBodies collects the generateContent request bodies a validating
// server receives, in request order.
type recordedBodies struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (r *recordedBodies) add(body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, body)
}

func (r *recordedBodies) get(t *testing.T, i int) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.bodies) {
		t.Fatalf("request %d: only %d requests were sent", i+1, len(r.bodies))
	}
	var body map[string]any
	if err := json.Unmarshal(r.bodies[i], &body); err != nil {
		t.Fatalf("request %d: %v", i+1, err)
	}
	return body
}

func (r *recordedBodies) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// scriptedServer serves the given generateContent responses in order through
// the validating server, recording each request body.
func scriptedServer(t *testing.T, cacheReqs *int, responses ...string) (string, *recordedBodies) {
	t.Helper()
	rec := &recordedBodies{}
	srv := newValidatingGenerateContentServer(t, cacheReqs, func(reqNum int, body []byte) string {
		rec.add(body)
		if reqNum > len(responses) {
			t.Errorf("unexpected request %d", reqNum)
			return submitTurnJSON
		}
		return responses[reqNum-1]
	})
	return srv.URL, rec
}

// askTool is the suspend tool's declaration.
func askTool() (*genai.FunctionDeclaration, error) {
	return &genai.FunctionDeclaration{
		Name:        "ask",
		Description: "Ask the operator a question and pause until they answer.",
		Parameters: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{"question": {Type: genai.TypeString}, "context": {Type: genai.TypeString}},
			Required:   []string{"question"},
		},
	}, nil
}

// acceptingSubmit is a submit tool that accepts every submission.
func acceptingSubmit() (googletool.SubmitMetadata[errCapResponse], error) {
	return googletool.SubmitMetadata[errCapResponse]{
		Definition: &genai.FunctionDeclaration{Name: "submit_result"},
		Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse]) toolcall.SubmitOutcome[errCapResponse] {
			answer, _ := call.Args["answer"].(string)
			return toolcall.SubmitOutcome[errCapResponse]{
				Accepted:   true,
				Response:   errCapResponse{Answer: answer},
				ToolResult: map[string]any{"success": true},
			}
		},
	}, nil
}

type suspendExecutor interface {
	googleexecutor.Interface[errCapRequest, errCapResponse]
	googleexecutor.Resumer[errCapRequest, errCapResponse]
	googleexecutor.ResultResumer[errCapRequest, errCapResponse]
}

// newSuspendExecutor builds an executor with the ask suspend tool, a submit
// tool and system instructions, so the context cache is in play.
func newSuspendExecutor(t *testing.T, url string, extra ...googleexecutor.Option[errCapRequest, errCapResponse]) suspendExecutor {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("find the version")
	if err != nil {
		t.Fatal(err)
	}
	sys, err := promptbuilder.NewPrompt("you are a careful agent")
	if err != nil {
		t.Fatal(err)
	}
	opts := append([]googleexecutor.Option[errCapRequest, errCapResponse]{
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
		googleexecutor.WithSystemInstructions[errCapRequest, errCapResponse](sys),
		googleexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](acceptingSubmit),
		googleexecutor.WithSuspendTool[errCapRequest, errCapResponse](askTool),
	}, extra...)
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](newTestClient(t, url), prompt, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	se, ok := exec.(suspendExecutor)
	if !ok {
		t.Fatal("the executor New returns is not a Resumer and ResultResumer")
	}
	return se
}

// lookupTools returns a lookup tool that answers with its query and counts
// its calls.
func lookupTools(calls *int, mu *sync.Mutex) map[string]googletool.Metadata[errCapResponse] {
	return map[string]googletool.Metadata[errCapResponse]{
		"lookup": {
			Definition: &genai.FunctionDeclaration{Name: "lookup"},
			Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) *genai.FunctionResponse {
				mu.Lock()
				*calls++
				mu.Unlock()
				return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{"found": call.Args["q"]}}
			},
		},
	}
}

// suspendOn runs Execute and returns its suspension, round-tripped through
// JSON as a store would hold it.
func suspendOn(t *testing.T, exec suspendExecutor, tools map[string]googletool.Metadata[errCapResponse]) *checkpoint.Suspension {
	t.Helper()
	_, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got %v, want a *checkpoint.Suspension", err)
	}
	raw, err := json.Marshal(susp)
	if err != nil {
		t.Fatal(err)
	}
	var stored checkpoint.Suspension
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	return &stored
}

// TestSuspendResumeContinuesTheSameConversation parks a run on the ask tool,
// stores the envelope, and resumes it on a fresh executor. The resumed request
// must carry the original prompt and the paused model turn unchanged (thought
// signatures included), followed by the siblings' real responses and the
// answer in call order. The fresh executor must create its own context cache.
func TestSuspendResumeContinuesTheSameConversation(t *testing.T) {
	var cacheReqs int
	url, rec := scriptedServer(t, &cacheReqs, parkTurnJSON, submitTurnJSON)
	var mu sync.Mutex
	var lookups int
	tools := lookupTools(&lookups, &mu)

	susp := suspendOn(t, newSuspendExecutor(t, url), tools)
	wantPending := []checkpoint.PendingToolCall{{ID: "ask#1", Name: "ask", InputJSON: json.RawMessage(`{"context":"both build","question":"which version?"}`)}}
	if diff := cmp.Diff(wantPending, susp.PendingToolCalls); diff != "" {
		t.Errorf("PendingToolCalls (-want +got):\n%s", diff)
	}
	if got, want := susp.Question, "which version?"; got != want {
		t.Errorf("Question: got = %q, want = %q", got, want)
	}
	if got, want := susp.Provider, checkpoint.ProviderGoogle; got != want {
		t.Errorf("Provider: got = %q, want = %q", got, want)
	}
	if got, want := susp.Turn, 0; got != want {
		t.Errorf("Turn: got = %d, want = %d", got, want)
	}
	if got, want := susp.RemainingTurns, 4; got != want {
		t.Errorf("RemainingTurns: got = %d, want = %d", got, want)
	}
	if lookups != 2 {
		t.Errorf("lookup calls before the suspension: got = %d, want = 2", lookups)
	}
	if strings.Contains(string(susp.ProviderState), "cachedContents/") {
		t.Error("ProviderState names a context cache; a resume must not depend on one the server may have expired")
	}

	resp, err := newSuspendExecutor(t, url).Resume(t.Context(), susp.Envelope, map[string]string{"ask#1": "use v2"}, tools)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("Answer: got = %q, want = %q", got, want)
	}
	if lookups != 2 {
		t.Errorf("lookup calls after the resume: got = %d, want = 2 (a resume must not rerun the paused turn's tools)", lookups)
	}
	if got, want := cacheReqs, 2; got != want {
		t.Errorf("cache creations: got = %d, want = %d (one per executor)", got, want)
	}

	first, resumed := rec.get(t, 0), rec.get(t, 1)
	var pausedTurn map[string]any
	if err := json.Unmarshal([]byte(parkTurnJSON), &pausedTurn); err != nil {
		t.Fatal(err)
	}
	modelContent := pausedTurn["candidates"].([]any)[0].(map[string]any)["content"]
	firstContents := first["contents"].([]any)
	resumedContents := resumed["contents"].([]any)
	if got, want := len(resumedContents), 3; got != want {
		t.Fatalf("resumed contents: got = %d, want = %d", got, want)
	}
	if diff := cmp.Diff(firstContents[0], resumedContents[0]); diff != "" {
		t.Errorf("resumed prompt (-first +resumed):\n%s", diff)
	}
	if diff := cmp.Diff(modelContent, resumedContents[1]); diff != "" {
		t.Errorf("resumed model turn must replay verbatim (-returned +resumed):\n%s", diff)
	}
	wantAnswer := map[string]any{"role": "user", "parts": []any{
		map[string]any{"functionResponse": map[string]any{"name": "lookup", "response": map[string]any{"found": "a"}}},
		map[string]any{"functionResponse": map[string]any{"name": "ask", "response": map[string]any{"output": checkpoint.FrameAnswer("use v2", 0)}}},
		map[string]any{"functionResponse": map[string]any{"name": "lookup", "response": map[string]any{"found": "b"}}},
	}}
	if diff := cmp.Diff(wantAnswer, resumedContents[2]); diff != "" {
		t.Errorf("resumed user turn (-want +got):\n%s", diff)
	}
	if got, want := resumed["cachedContent"], "cachedContents/test-cache"; got != want {
		t.Errorf("resumed cachedContent: got = %v, want = %q", got, want)
	}
}

// TestResumeWithResultsPlacesResultsVerbatim: a caller-framed result reaches
// the model as it is, and a pending call without a result fails before any
// request is sent.
func TestResumeWithResultsPlacesResultsVerbatim(t *testing.T) {
	url, rec := scriptedServer(t, nil, parkTurnJSON, submitTurnJSON)
	var mu sync.Mutex
	var lookups int
	tools := lookupTools(&lookups, &mu)
	susp := suspendOn(t, newSuspendExecutor(t, url), tools)

	if _, err := newSuspendExecutor(t, url).ResumeWithResults(t.Context(), susp.Envelope, map[string]string{"other": "x"}, tools); err == nil {
		t.Fatal("ResumeWithResults without a result for the pending call: got nil, want error")
	}
	if got, want := rec.count(), 1; got != want {
		t.Fatalf("requests after a refused resume: got = %d, want = %d", got, want)
	}

	const result = `{"refused":"not now"}`
	if _, err := newSuspendExecutor(t, url).ResumeWithResults(t.Context(), susp.Envelope, map[string]string{"ask#1": result}, tools); err != nil {
		t.Fatalf("ResumeWithResults: %v", err)
	}
	parts := rec.get(t, 1)["contents"].([]any)[2].(map[string]any)["parts"].([]any)
	got := parts[1].(map[string]any)["functionResponse"].(map[string]any)["response"]
	if diff := cmp.Diff(map[string]any{"output": result}, got); diff != "" {
		t.Errorf("pending call's response (-want +got):\n%s", diff)
	}
}

// TestSubmitAndSuspendSameTurn: a committed submission in the same turn makes
// the question moot, so the run ends with the result.
func TestSubmitAndSuspendSameTurn(t *testing.T) {
	url, _ := scriptedServer(t, nil, askAndSubmitTurnJSON)
	resp, err := newSuspendExecutor(t, url).Execute(t.Context(), errCapRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("Answer: got = %q, want = %q", got, want)
	}
}

// signatureOnlyParkTurnJSON calls the ask tool alongside a part carrying only
// a thought signature, which the SDK's validateContent rejects, so the turn
// never reaches the curated history.
const signatureOnlyParkTurnJSON = `{
	"candidates":[{"content":{"role":"model","parts":[
		{"thoughtSignature":"c2lnLW9ubHk="},
		{"functionCall":{"name":"ask","args":{"question":"which version?"}}}
	]}}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
}`

// TestSuspendFailsWhenTheTurnIsNotInTheCuratedHistory: a paused turn the SDK
// left out of the curated history fails the park rather than storing a
// history that ends with the previous model turn, which resume would pair the
// answer against.
func TestSuspendFailsWhenTheTurnIsNotInTheCuratedHistory(t *testing.T) {
	url, _ := scriptedServer(t, nil, lookupTurnJSON, signatureOnlyParkTurnJSON)
	var mu sync.Mutex
	var lookups int
	_, err := newSuspendExecutor(t, url).Execute(t.Context(), errCapRequest{}, lookupTools(&lookups, &mu))
	if err == nil {
		t.Fatal("Execute: got nil error, want the park to fail")
	}
	if susp, ok := checkpoint.AsSuspension(err); ok {
		t.Fatalf("Execute: got suspension pending %v, want an error", susp.PendingToolCalls)
	}
	if !strings.Contains(err.Error(), "curated chat history does not end with the model turn") {
		t.Errorf("Execute: got %v, want the curated history error", err)
	}
}

// TestResumeFailsClosedOnConfigDrift: an envelope resumed against a different
// configuration returns checkpoint.ErrConfigDrift and sends nothing.
func TestResumeFailsClosedOnConfigDrift(t *testing.T) {
	otherSys, err := promptbuilder.NewPrompt("you are a different agent")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lookups int
	tools := lookupTools(&lookups, &mu)

	for _, tc := range []struct {
		name  string
		opts  []googleexecutor.Option[errCapRequest, errCapResponse]
		tools map[string]googletool.Metadata[errCapResponse]
	}{{
		name:  "system instructions",
		opts:  []googleexecutor.Option[errCapRequest, errCapResponse]{googleexecutor.WithSystemInstructions[errCapRequest, errCapResponse](otherSys)},
		tools: tools,
	}, {
		name: "tool set",
		tools: map[string]googletool.Metadata[errCapResponse]{
			"lookup": tools["lookup"],
			"grep":   {Definition: &genai.FunctionDeclaration{Name: "grep"}, Handler: tools["lookup"].Handler},
		},
	}, {
		name:  "model",
		opts:  []googleexecutor.Option[errCapRequest, errCapResponse]{googleexecutor.WithModel[errCapRequest, errCapResponse]("gemini-2.5-pro")},
		tools: tools,
	}, {
		name:  "suspend on submit",
		opts:  []googleexecutor.Option[errCapRequest, errCapResponse]{googleexecutor.WithSuspendOnSubmit[errCapRequest, errCapResponse]()},
		tools: tools,
	}, {
		name:  "temperature",
		opts:  []googleexecutor.Option[errCapRequest, errCapResponse]{googleexecutor.WithTemperature[errCapRequest, errCapResponse](0.9)},
		tools: tools,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			url, rec := scriptedServer(t, nil, parkTurnJSON)
			susp := suspendOn(t, newSuspendExecutor(t, url), tools)
			_, err := newSuspendExecutor(t, url, tc.opts...).Resume(t.Context(), susp.Envelope, map[string]string{"ask#1": "v2"}, tc.tools)
			if !errors.Is(err, checkpoint.ErrConfigDrift) {
				t.Fatalf("Resume: got %v, want checkpoint.ErrConfigDrift", err)
			}
			if got, want := rec.count(), 1; got != want {
				t.Errorf("requests: got = %d, want = %d (a drifted resume must send nothing)", got, want)
			}
		})
	}
}

// TestResumeLabelsAreNotConfigDrift: resource labels attribute billing and do
// not change what the model sees, so they stay out of the digest.
func TestResumeLabelsAreNotConfigDrift(t *testing.T) {
	url, _ := scriptedServer(t, nil, parkTurnJSON, submitTurnJSON)
	var mu sync.Mutex
	var lookups int
	tools := lookupTools(&lookups, &mu)
	susp := suspendOn(t, newSuspendExecutor(t, url), tools)
	resumer := newSuspendExecutor(t, url, googleexecutor.WithResourceLabels[errCapRequest, errCapResponse](map[string]string{"team": "other"}))
	if _, err := resumer.Resume(t.Context(), susp.Envelope, map[string]string{"ask#1": "v2"}, tools); err != nil {
		t.Fatalf("Resume: %v", err)
	}
}

// TestSuspendToolCollisions: the suspend tool may share a name with neither
// the submit tool nor a caller tool.
func TestSuspendToolCollisions(t *testing.T) {
	prompt, err := promptbuilder.NewPrompt("p")
	if err != nil {
		t.Fatal(err)
	}
	_, err = googleexecutor.New[errCapRequest, errCapResponse](newTestClient(t, "http://127.0.0.1:0"), prompt,
		googleexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](acceptingSubmit),
		googleexecutor.WithSuspendTool[errCapRequest, errCapResponse](func() (*genai.FunctionDeclaration, error) {
			return &genai.FunctionDeclaration{Name: "submit_result"}, nil
		}))
	if err == nil {
		t.Error("New with a suspend tool named like the submit tool: got nil, want error")
	}

	url, rec := scriptedServer(t, nil)
	tools := map[string]googletool.Metadata[errCapResponse]{"ask": {Definition: &genai.FunctionDeclaration{Name: "ask"}}}
	if _, err := newSuspendExecutor(t, url).Execute(t.Context(), errCapRequest{}, tools); err == nil {
		t.Error("Execute with a caller tool named like the suspend tool: got nil, want error")
	}
	if got := rec.count(); got != 0 {
		t.Errorf("requests: got = %d, want = 0", got)
	}
}

// TestSuspendOnSubmitResumesTheSameConversation: each accepted submission
// suspends with the submit call pending, and ResumeWithResults sends the
// review as that call's response in the same conversation.
func TestSuspendOnSubmitResumesTheSameConversation(t *testing.T) {
	const secondSubmit = `{
		"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_submit_2","name":"submit_result","args":{"answer":"43"}}}]}}],
		"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
	}`
	url, rec := scriptedServer(t, nil, submitTurnJSON, secondSubmit)
	opt := googleexecutor.WithSuspendOnSubmit[errCapRequest, errCapResponse]()

	resp, err := newSuspendExecutor(t, url, opt).Execute(t.Context(), errCapRequest{}, nil)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok || susp.Reason != checkpoint.ReasonSubmitted {
		t.Fatalf("Execute: got %v, want a %q suspension", err, checkpoint.ReasonSubmitted)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("Execute's Response: got = %q, want = %q", got, want)
	}
	if got := susp.PendingToolCalls; len(got) != 1 || got[0].ID != "call_submit" {
		t.Fatalf("PendingToolCalls: got %+v, want the submit call", got)
	}

	const review = "name the gap"
	resp, err = newSuspendExecutor(t, url, opt).ResumeWithResults(t.Context(), susp.Envelope, map[string]string{"call_submit": review}, nil)
	again, ok := checkpoint.AsSuspension(err)
	if !ok || again.PendingToolCalls[0].ID != "call_submit_2" {
		t.Fatalf("ResumeWithResults: got %v, want the second submission's suspension", err)
	}
	if got, want := resp.Answer, "43"; got != want {
		t.Errorf("resumed Response: got = %q, want = %q", got, want)
	}
	last := rec.get(t, 1)["contents"].([]any)[2]
	want := map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{
		"id": "call_submit", "name": "submit_result", "response": map[string]any{"output": review},
	}}}}
	if diff := cmp.Diff(want, last); diff != "" {
		t.Errorf("resumed user turn (-want +got):\n%s", diff)
	}
}

// TestSuspendOnSubmitAnswersASiblingAsk: a suspending submission makes a
// sibling ask moot, and the ask still gets a response so the resumed request
// pairs every call.
func TestSuspendOnSubmitAnswersASiblingAsk(t *testing.T) {
	url, rec := scriptedServer(t, nil, askAndSubmitTurnJSON, submitTurnJSON)
	opt := googleexecutor.WithSuspendOnSubmit[errCapRequest, errCapResponse]()
	_, err := newSuspendExecutor(t, url, opt).Execute(t.Context(), errCapRequest{}, nil)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok || susp.Reason != checkpoint.ReasonSubmitted {
		t.Fatalf("Execute: got %v, want a %q suspension", err, checkpoint.ReasonSubmitted)
	}
	_, err = newSuspendExecutor(t, url, opt).ResumeWithResults(t.Context(), susp.Envelope, map[string]string{"call_submit": "ok"}, nil)
	if _, ok := checkpoint.AsSuspension(err); !ok {
		t.Fatalf("ResumeWithResults: got %v, want the next submission's suspension", err)
	}
	parts := rec.get(t, 1)["contents"].([]any)[2].(map[string]any)["parts"].([]any)
	ask := parts[0].(map[string]any)["functionResponse"].(map[string]any)
	if got, want := ask["response"].(map[string]any)["status"], "not_asked"; got != want {
		t.Errorf("ask response status: got = %v, want = %q", got, want)
	}
}

// TestSuspendOnSubmitSeedSuspends: an accepted seeded submission suspends
// before the first request, with the ID-less seed call pending under its
// derived ID, and a resume continues the conversation from the seed call.
func TestSuspendOnSubmitSeedSuspends(t *testing.T) {
	url, rec := scriptedServer(t, nil, submitTurnJSON)
	opt := googleexecutor.WithSuspendOnSubmit[errCapRequest, errCapResponse]()

	resp, err := newSuspendExecutor(t, url, opt).Execute(t.Context(), errCapRequest{}, nil,
		&genai.FunctionCall{Name: "submit_result", Args: map[string]any{"answer": "seeded"}})
	susp, ok := checkpoint.AsSuspension(err)
	if !ok || susp.Reason != checkpoint.ReasonSubmitted {
		t.Fatalf("Execute: got %v, want a %q suspension", err, checkpoint.ReasonSubmitted)
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("requests before the seeded suspension: got = %d, want = 0", got)
	}
	if got, want := resp.Answer, "seeded"; got != want {
		t.Errorf("Execute's Response: got = %q, want = %q", got, want)
	}
	if got := susp.PendingToolCalls; len(got) != 1 || got[0].ID != "submit_result#0" {
		t.Fatalf("PendingToolCalls: got %+v, want the seed call as submit_result#0", got)
	}
	if got, want := susp.Turn, -1; got != want {
		t.Errorf("Turn: got = %d, want = %d", got, want)
	}
	if got, want := susp.RemainingTurns, 5; got != want {
		t.Errorf("RemainingTurns: got = %d, want = %d", got, want)
	}

	const review = "name the gap"
	_, err = newSuspendExecutor(t, url, opt).ResumeWithResults(t.Context(), susp.Envelope, map[string]string{"submit_result#0": review}, nil)
	if again, ok := checkpoint.AsSuspension(err); !ok || again.Turn != 0 {
		t.Fatalf("ResumeWithResults: got %v, want the next submission's suspension at turn 0", err)
	}
	want := []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "find the version"}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{
			"name": "submit_result", "args": map[string]any{"answer": "seeded"},
		}}}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{
			"name": "submit_result", "response": map[string]any{"output": review},
		}}}},
	}
	if diff := cmp.Diff(want, rec.get(t, 0)["contents"]); diff != "" {
		t.Errorf("resumed contents (-want +got):\n%s", diff)
	}
}

// TestSecondPendingCallInATurnGetsAnError: only a turn's first suspend call,
// or under WithSuspendOnSubmit its first submission, is pending. A later one
// gets an error response, which the resumed request carries so every call is
// answered.
func TestSecondPendingCallInATurnGetsAnError(t *testing.T) {
	const twoAsks = `{
		"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"id":"call_ask_1","name":"ask","args":{"question":"first?"}}},
			{"functionCall":{"id":"call_ask_2","name":"ask","args":{"question":"second?"}}}
		]}}],
		"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
	}`
	const twoSubmits = `{
		"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"id":"call_submit_1","name":"submit_result","args":{"answer":"42"}}},
			{"functionCall":{"id":"call_submit_2","name":"submit_result","args":{"answer":"43"}}}
		]}}],
		"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
	}`
	for _, tc := range []struct {
		name      string
		turn      string
		opts      []googleexecutor.Option[errCapRequest, errCapResponse]
		pending   string
		wantError string
	}{{
		name:      "two asks",
		turn:      twoAsks,
		pending:   "call_ask_1",
		wantError: "only one question per turn is asked",
	}, {
		name:      "two submissions",
		turn:      twoSubmits,
		opts:      []googleexecutor.Option[errCapRequest, errCapResponse]{googleexecutor.WithSuspendOnSubmit[errCapRequest, errCapResponse]()},
		pending:   "call_submit_1",
		wantError: "only one submission per turn is reviewed",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			url, rec := scriptedServer(t, nil, tc.turn, submitTurnJSON)
			susp := suspendOn(t, newSuspendExecutor(t, url, tc.opts...), nil)
			if got := susp.PendingToolCalls; len(got) != 1 || got[0].ID != tc.pending {
				t.Fatalf("PendingToolCalls: got %+v, want only %s", got, tc.pending)
			}
			_, err := newSuspendExecutor(t, url, tc.opts...).ResumeWithResults(t.Context(), susp.Envelope, map[string]string{tc.pending: "answer"}, nil)
			if _, ok := checkpoint.AsSuspension(err); err != nil && !ok {
				t.Fatalf("ResumeWithResults: %v", err)
			}
			parts := rec.get(t, 1)["contents"].([]any)[2].(map[string]any)["parts"].([]any)
			if got, want := len(parts), 2; got != want {
				t.Fatalf("resumed function responses: got = %d, want = %d", got, want)
			}
			second := parts[1].(map[string]any)["functionResponse"].(map[string]any)
			msg, _ := second["response"].(map[string]any)["error"].(string)
			if !strings.Contains(msg, tc.wantError) {
				t.Errorf("second call's response: got %v, want an error containing %q", second["response"], tc.wantError)
			}
		})
	}
}

// TestResumeCapsTheBudgetAtTheLiveMaxTurns: an envelope whose remaining
// budget exceeds the resuming executor's maxTurns stops at the live cap.
func TestResumeCapsTheBudgetAtTheLiveMaxTurns(t *testing.T) {
	const lookupTurn = `{
		"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_lookup","name":"lookup","args":{"q":"c"}}}]}}],
		"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
	}`
	url, rec := scriptedServer(t, nil, parkTurnJSON, lookupTurn, lookupTurn)
	var mu sync.Mutex
	var lookups int
	tools := lookupTools(&lookups, &mu)
	susp := suspendOn(t, newSuspendExecutor(t, url), tools)
	if got, want := susp.RemainingTurns, 4; got != want {
		t.Fatalf("RemainingTurns: got = %d, want = %d", got, want)
	}

	resumer := newSuspendExecutor(t, url, googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](1))
	_, err := resumer.Resume(t.Context(), susp.Envelope, map[string]string{"ask#1": "v2"}, tools)
	if !errors.Is(err, agentexecutor.ErrMaxTurns) {
		t.Fatalf("Resume: got %v, want agentexecutor.ErrMaxTurns", err)
	}
	// The park, one budgeted turn, and the synthetic turn that sends the
	// last tool responses.
	if got, want := rec.count(), 3; got != want {
		t.Errorf("requests: got = %d, want = %d", got, want)
	}
}

// TestResuspendCarriesTheRunsRemainingBudget: a resumed run that parks again
// records what is left of the budget it resumed with, whatever maxTurns the
// resuming executor has. The first park (maxTurns 5, turn 0) leaves 4 turns;
// the resume parks again on its first turn (turn 1), leaving 3 of them, or
// fewer when the live cap is lower.
func TestResuspendCarriesTheRunsRemainingBudget(t *testing.T) {
	tests := []struct {
		name          string
		liveMaxTurns  int
		wantRemaining int
	}{{
		name:          "raised cap",
		liveMaxTurns:  10,
		wantRemaining: 3,
	}, {
		name:          "lowered cap",
		liveMaxTurns:  2,
		wantRemaining: 1,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, _ := scriptedServer(t, nil, parkTurnJSON, parkTurnJSON)
			var mu sync.Mutex
			var lookups int
			tools := lookupTools(&lookups, &mu)
			first := suspendOn(t, newSuspendExecutor(t, url), tools)
			if got, want := first.RemainingTurns, 4; got != want {
				t.Fatalf("first RemainingTurns: got = %d, want = %d", got, want)
			}

			resumer := newSuspendExecutor(t, url, googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](tt.liveMaxTurns))
			_, err := resumer.Resume(t.Context(), first.Envelope, map[string]string{"ask#1": "v2"}, tools)
			second, ok := checkpoint.AsSuspension(err)
			if !ok {
				t.Fatalf("Resume: got %v, want a *checkpoint.Suspension", err)
			}
			if got, want := second.Turn, 1; got != want {
				t.Errorf("second Turn: got = %d, want = %d", got, want)
			}
			if got, want := second.RemainingTurns, tt.wantRemaining; got != want {
				t.Errorf("second RemainingTurns: got = %d, want = %d", got, want)
			}
		})
	}
}

// TestSuspendOnSubmitRequiresASubmitTool fails construction without a submit
// tool.
func TestSuspendOnSubmitRequiresASubmitTool(t *testing.T) {
	prompt, err := promptbuilder.NewPrompt("p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := googleexecutor.New[errCapRequest, errCapResponse](newTestClient(t, "http://127.0.0.1:0"), prompt,
		googleexecutor.WithSuspendOnSubmit[errCapRequest, errCapResponse]()); err == nil {
		t.Error("New with WithSuspendOnSubmit and no submit tool: got nil, want error")
	}
}
