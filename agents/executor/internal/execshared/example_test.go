/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/executor/internal/execshared"
	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
)

func ExampleAppendUserPromptSuffix() {
	suffix, err := promptbuilder.NewPrompt("Focus on error handling.")
	if err != nil {
		panic(err)
	}
	prompt, err := execshared.AppendUserPromptSuffix("changeset payload", suffix)
	if err != nil {
		panic(err)
	}
	fmt.Println(prompt)
	// Output:
	// changeset payload
	//
	// Focus on error handling.
}

func ExampleDefaultResourceLabels() {
	// Custom labels override the environment-derived defaults on key match.
	labels := execshared.DefaultResourceLabels(map[string]string{"team": "platform"})
	fmt.Println(labels["team"])
	// Output:
	// platform
}

func ExampleSubmitPredicate() {
	tools := map[string]struct{}{"read_file": {}}
	isSubmit := execshared.SubmitPredicate(tools, "submit_result", true)
	fmt.Println(isSubmit("submit_result"), isSubmit("read_file"), isSubmit("other"))
	// Output:
	// true false false
}

func ExampleDispatchToolCalls() {
	// Submit calls are held out of the pool and run only after every other
	// handler has finished; a concurrency of 1 runs the pooled calls
	// strictly in order.
	calls := []string{"submit_result", "read_file", "list_dir"}
	var order []string
	execshared.DispatchToolCalls(calls, 1,
		func(call string) bool { return call == "submit_result" },
		func(_ int, call string) { order = append(order, call) },
	)
	fmt.Println(order)
	// Output:
	// [read_file list_dir submit_result]
}

func ExampleGateSubmission() {
	type reviewResult struct {
		Verdict string `json:"verdict"`
	}

	ctx := context.Background()
	trace, _ := agenttrace.StartTrace[reviewResult](ctx, "prompt")
	rec := telemetry.NewRecorder(metrics.NewGenAI("example"), "model", "provider", nil, nil)

	// The backend's submit handler parsed the model's call into an accepted
	// outcome; the gate runs the validators and commits the response.
	var result reviewResult
	toolResult, committed, err := execshared.GateSubmission(ctx,
		toolcall.SubmitOutcome[reviewResult]{
			Accepted:   true,
			Response:   reviewResult{Verdict: "pass"},
			Reasoning:  "all checks passed",
			ToolResult: map[string]any{"success": true},
		},
		trace, "call-1", "submit_result", map[string]any{"verdict": "pass"},
		nil, rec, "submit_result", &result)
	if err != nil {
		panic(err)
	}
	fmt.Println(committed, result.Verdict, toolResult["success"])
	// Output:
	// true pass true
}

func ExampleValidateSuspendToolName() {
	fmt.Println(execshared.ValidateSuspendToolName("ask_a_friend", "submit_result"))
	fmt.Println(execshared.ValidateSuspendToolName("submit_result", "submit_result"))
	// Output:
	// <nil>
	// suspend tool "submit_result" collides with the submit tool name; the run could never terminate
}

func ExampleHeldOutPartition() {
	// The suspend tool joins the submit tool in the held-out set; a
	// caller-registered tool is dispatched in the concurrent pool.
	tools := map[string]struct{}{"read_file": {}}
	isSubmit, isSuspend, heldOut, err := execshared.HeldOutPartition(tools, "submit_result", true, "ask_a_friend", true)
	if err != nil {
		panic(err)
	}
	fmt.Println(isSubmit("submit_result"), isSuspend("ask_a_friend"), heldOut("read_file"))
	// Output:
	// true true false
}

func ExampleFailTurnUnlessSuspended() {
	ctx := context.Background()
	trace, _ := agenttrace.StartTrace[string](ctx, "prompt")
	llmTurn := trace.BeginTurn(0, "anthropic", "model")
	defer llmTurn.End()

	// A suspension is an intentional halt, not a failure: the turn is left
	// unfailed. Any other error would mark the turn failed.
	execshared.FailTurnUnlessSuspended(llmTurn, &checkpoint.Suspension{})
	fmt.Println("turn not failed")
	// Output:
	// turn not failed
}

func ExampleNewArgLogRedactor() {
	// No tool declares a policy, so the run logs every argument value.
	unpoliced := execshared.NewArgLogRedactor(map[string]*toolcall.ArgLogPolicy{"git_grep": nil})
	fmt.Println(unpoliced.AppendArgs(nil, "git_grep",
		map[string]any{"pattern": "restricted-input-7742"})...)

	// One tool declaring a policy policies the whole run, so a sibling that
	// declared none has its arguments withheld and counted.
	policed := execshared.NewArgLogRedactor(map[string]*toolcall.ArgLogPolicy{
		"git_grep":       toolcall.NewArgLogPolicy("ref"),
		"git_log_search": nil,
	})
	fmt.Println(policed.AppendArgs(nil, "git_log_search",
		map[string]any{"pattern": "restricted-input-7742"})...)

	// Output:
	// args.pattern restricted-input-7742
	// args_withheld 1
}

func ExampleArgLogRedactor_AppendArgs() {
	redactor := execshared.NewArgLogRedactor(map[string]*toolcall.ArgLogPolicy{
		"git_grep": toolcall.NewArgLogPolicy("ref"),
	})

	// An allowed argument keeps its name and value; anything else is counted,
	// because the model chooses the names too. One call per argument here:
	// AppendArgs walks a map, so several at once have no stable order.
	fmt.Println(redactor.AppendArgs(nil, "git_grep", map[string]any{"ref": "4.1.0"})...)
	fmt.Println(redactor.AppendArgs(nil, "git_grep",
		map[string]any{"reasoning": "confirm the caller input still appears at 4.1.0"})...)

	// Output:
	// args.ref 4.1.0
	// args_withheld 1
}

func ExampleArgLogRedactor_ToolName() {
	// The held-out submit tool is named separately. It dispatches from outside
	// the policy map, so leaving it out would make a real call read as unknown.
	redactor := execshared.NewArgLogRedactor(map[string]*toolcall.ArgLogPolicy{
		"git_grep": toolcall.NewArgLogPolicy("ref"),
	}, "submit_result")

	// The provider reports the name the model emitted, so a name the run never
	// registered is model text and is replaced rather than logged.
	fmt.Println(redactor.ToolName("git_grep"))
	fmt.Println(redactor.ToolName("submit_result"))
	fmt.Println(redactor.ToolName("the caller input names lib/index.ts"))

	// Output:
	// git_grep
	// submit_result
	// [unknown]
}
