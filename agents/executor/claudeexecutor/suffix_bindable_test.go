/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"encoding/json"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// lensRequest binds a shared payload into the user prompt and its own lens
// into the suffix.
type lensRequest struct {
	Payload string
	Lens    string
}

type lensText struct {
	Text string `xml:",chardata"`
}

func (r lensRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p.BindXML("payload", lensText{Text: r.Payload})
}

func (r lensRequest) BindSuffix(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p.BindXML("lens", lensText{Text: r.Lens})
}

// TestSuffixBindableRequestVariesTheSuffixOnly: two executions of one
// executor send byte-identical leading blocks, each marked as the cache
// breakpoint, and differ only in the suffix block each request bound.
func TestSuffixBindableRequestVariesTheSuffixOnly(t *testing.T) {
	client, requests := newExecTestClient(t, func(int) []string { return answerTurn })
	exec, err := claudeexecutor.New[lensRequest, errCapResponse](
		client,
		promptbuilder.MustNewPrompt("{{payload}}"),
		claudeexecutor.WithUserPromptSuffix[lensRequest, errCapResponse](promptbuilder.MustNewPrompt("Lens: {{lens}}")),
		claudeexecutor.WithRetryConfig[lensRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[lensRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, lens := range []string{"security", "tests"} {
		if _, err := exec.Execute(t.Context(), lensRequest{Payload: "the change", Lens: lens}, nil); err != nil {
			t.Fatalf("Execute(%s): %v", lens, err)
		}
	}

	type block struct {
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	var got [][]block
	for _, body := range requests() {
		var req struct {
			Messages []struct {
				Content []block `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decoding the request: %v", err)
		}
		got = append(got, req.Messages[0].Content)
	}
	if len(got) != 2 {
		t.Fatalf("requests: got = %d, want = 2", len(got))
	}
	for i, lens := range []string{"security", "tests"} {
		if len(got[i]) != 2 {
			t.Fatalf("request %d: got %d blocks, want 2", i, len(got[i]))
		}
		if want := "<lensText>the change</lensText>"; got[i][0].Text != want {
			t.Errorf("request %d leading block: got = %q, want = %q", i, got[i][0].Text, want)
		}
		if len(got[i][0].CacheControl) == 0 {
			t.Errorf("request %d: the leading block carries no cache breakpoint", i)
		}
		if want := "Lens: <lensText>" + lens + "</lensText>"; got[i][1].Text != want {
			t.Errorf("request %d suffix: got = %q, want = %q", i, got[i][1].Text, want)
		}
	}
}

// TestSuffixLeftUnboundForOtherRequests: a request that is not
// SuffixBindable leaves the suffix as configured, so a placeholder in it
// fails the execution rather than reaching the model unbound.
func TestSuffixLeftUnboundForOtherRequests(t *testing.T) {
	client, requests := newExecTestClient(t, func(int) []string { return answerTurn })
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client,
		promptbuilder.MustNewPrompt("hello"),
		claudeexecutor.WithUserPromptSuffix[errCapRequest, errCapResponse](promptbuilder.MustNewPrompt("Lens: {{lens}}")),
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := exec.Execute(t.Context(), errCapRequest{}, nil); err == nil {
		t.Error("Execute: got nil error for an unbound suffix placeholder")
	}
	if n := len(requests()); n != 0 {
		t.Errorf("requests: got = %d, want = 0", n)
	}
}
