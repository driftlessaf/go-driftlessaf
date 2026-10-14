/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// TestExecuteNoCandidatesError checks that a response without candidates
// reports the prompt block reason when the provider gives one.
func TestExecuteNoCandidatesError(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr []string
	}{{
		name: "prompt blocked",
		// The shape Gemini 4 Argon returned on 2026-10-13 after it generated
		// to the per-request output cap.
		body:    `{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"The prompt is blocked due to safety"},"usageMetadata":{"promptTokenCount":105148,"candidatesTokenCount":32768,"totalTokenCount":137916}}`,
		wantErr: []string{"prompt blocked", "SAFETY", "The prompt is blocked due to safety"},
	}, {
		name:    "no feedback",
		body:    `{"usageMetadata":{"promptTokenCount":10,"totalTokenCount":10}}`,
		wantErr: []string{"no candidates"},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)

			prompt, err := promptbuilder.NewPrompt("hello")
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			exec, err := googleexecutor.New[errCapRequest, errCapResponse](
				newTestClient(t, srv.URL),
				prompt,
				googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
				googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = exec.Execute(t.Context(), errCapRequest{}, nil)
			if err == nil {
				t.Fatal("Execute: got nil error, want a no-content error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Execute error: got = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}
