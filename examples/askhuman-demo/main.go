/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Command askhuman-demo drives the durable suspend/resume lifecycle end-to-end
// against real Claude on Vertex AI and local files, one subcommand per
// lifecycle step so each phase runs in its own OS process:
//
//	askhuman-demo ask     — run the agent; it must ask a human before it can
//	                        submit, so it suspends: the checkpoint envelope and
//	                        the pending question land as two local files.
//	askhuman-demo status  — pretty-print the parked envelope + question.
//	askhuman-demo resume  — tri-state wake: before an answer exists this is a
//	                        cheap re-arm (no model call, nothing mutated).
//	askhuman-demo answer  — record the human's answer next to the question.
//	askhuman-demo resume  — claims the checkpoint (CAS), rebuilds a fresh
//	                        executor, injects the answer, runs to completion,
//	                        and consumes both records.
//
// The stores are the local-file demo grade: a jsonlstore for envelopes and a
// JSON file for the question/answer transport. The GCS-backed checkpoint store
// (and its KMS-sealed envelopes) lands with the gcsstore slice of DEV-2247;
// swapping it in changes only the two store constructors in main.
//
// Configuration (env): GCP_PROJECT_ID (required), GCP_REGION (default global),
// AGENT_MODEL (default claude-sonnet-4-6 — must be claude-*: suspend/resume is
// only wired for the Claude backend today), CHECKPOINT_PATH / QUESTIONS_PATH
// (default under /tmp/askhuman-demo), DEMO_KEY (default deploy/billing-api).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/jsonlstore"
	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/suspend"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/sethvargo/go-envconfig"
)

var env = envconfig.MustProcess(context.Background(), &struct {
	Project        string `env:"GCP_PROJECT_ID,required"`
	Region         string `env:"GCP_REGION,default=global"`
	Model          string `env:"AGENT_MODEL,default=claude-sonnet-4-6"`
	CheckpointPath string `env:"CHECKPOINT_PATH,default=/tmp/askhuman-demo/checkpoints.jsonl"`
	QuestionsPath  string `env:"QUESTIONS_PATH,default=/tmp/askhuman-demo/questions.json"`
	Key            string `env:"DEMO_KEY,default=deploy/billing-api"`
}{})

// DeployRequest is the agent's task input.
type DeployRequest struct {
	Service string `json:"service"`
	Version string `json:"version"`
}

// Bind implements promptbuilder.Bindable.
func (r *DeployRequest) Bind(prompt *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return prompt.BindXML("deploy_request", r)
}

// DeployPlan is the agent's submitted result.
type DeployPlan struct {
	Environment string `json:"environment" jsonschema:"description=The deployment target environment the human approved"`
	Summary     string `json:"summary" jsonschema:"description=One-sentence summary of the planned deployment"`
}

var systemInstructions = promptbuilder.MustNewPrompt(`ROLE: deployment planner.
You are finalizing a deployment plan. You do NOT know the target environment
and MUST NOT guess it: call the ask_human tool exactly once to obtain it from
the operator, then call submit_result with the environment the human named.`)

var userPrompt = promptbuilder.MustNewPrompt(`Plan the deployment described in
<deploy_request>. Ask the operator which environment to target, then submit.

{{deploy_request}}`)

const (
	askHumanTool     = "ask_human"
	askHumanToolDesc = "Ask the human operator a question and pause until they answer. The conversation resumes with their answer as this call's result."
	wakeInterval     = time.Minute
)

// fileQuestions is a QuestionStore keeping every key's pending question (and
// its answer slot) in one local JSON file, so a demo audience can watch both
// halves of the pause on disk and so each subcommand process sees the state
// the previous one left behind.
type fileQuestions struct {
	path string
}

type questionDoc struct {
	Question suspend.Question `json:"question"`
	Answer   *suspend.Answer  `json:"answer,omitempty"`
}

var _ suspend.QuestionStore = (*fileQuestions)(nil)

func (q *fileQuestions) load() (map[string]questionDoc, error) {
	b, err := os.ReadFile(q.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]questionDoc{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]questionDoc{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("corrupt question file %q: %w", q.path, err)
	}
	return m, nil
}

func (q *fileQuestions) store(m map[string]questionDoc) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(q.path, b, 0o600)
}

// read returns the question doc for key, or nil when none exists.
func (q *fileQuestions) read(key string) (*questionDoc, error) {
	m, err := q.load()
	if err != nil {
		return nil, err
	}
	doc, ok := m[key]
	if !ok {
		return nil, nil
	}
	return &doc, nil
}

// Ask implements suspend.QuestionStore.
func (q *fileQuestions) Ask(_ context.Context, key string, question suspend.Question) error {
	m, err := q.load()
	if err != nil {
		return err
	}
	m[key] = questionDoc{Question: question}
	return q.store(m)
}

// Pending implements suspend.QuestionStore.
func (q *fileQuestions) Pending(_ context.Context, key string) (suspend.Question, bool, error) {
	doc, err := q.read(key)
	if err != nil || doc == nil {
		return suspend.Question{}, false, err
	}
	return doc.Question, true, nil
}

// Answer implements suspend.QuestionStore.
func (q *fileQuestions) Answer(_ context.Context, key string) (suspend.Answer, bool, error) {
	doc, err := q.read(key)
	if err != nil || doc == nil || doc.Answer == nil {
		return suspend.Answer{}, false, err
	}
	if doc.Answer.QuestionID != doc.Question.ID {
		return suspend.Answer{}, false, nil // stale answer, bound to a superseded pause
	}
	return *doc.Answer, true, nil
}

// Consume implements suspend.QuestionStore.
func (q *fileQuestions) Consume(_ context.Context, key, questionID string) error {
	m, err := q.load()
	if err != nil {
		return err
	}
	doc, ok := m[key]
	if !ok || doc.Question.ID != questionID {
		return nil // superseded pause: leave the live question alone
	}
	delete(m, key)
	return q.store(m)
}

// provide records a human answer bound to the pending question's nonce. It is
// the demo's stand-in for a real human-transport ingress.
func (q *fileQuestions) provide(key, text string) (suspend.Question, error) {
	m, err := q.load()
	if err != nil {
		return suspend.Question{}, err
	}
	doc, ok := m[key]
	if !ok {
		return suspend.Question{}, fmt.Errorf("no pending question for key %q", key)
	}
	doc.Answer = &suspend.Answer{QuestionID: doc.Question.ID, Text: text, AnsweredAt: time.Now()}
	m[key] = doc
	return doc.Question, q.store(m)
}

func newAgent(ctx context.Context) (metaagent.Agent[*DeployRequest, *DeployPlan, toolcall.EmptyTools], error) {
	return metaagent.New[*DeployRequest, *DeployPlan, toolcall.EmptyTools](
		ctx, env.Project, env.Region, env.Model,
		metaagent.Config[*DeployPlan, toolcall.EmptyTools]{
			SystemInstructions:     systemInstructions,
			UserPrompt:             userPrompt,
			Tools:                  toolcall.NewEmptyToolsProvider[*DeployPlan](),
			MaxTurns:               8,
			SuspendToolName:        askHumanTool,
			SuspendToolDescription: askHumanToolDesc,
		})
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: askhuman-demo ask|status|answer <text>|resume|clean")
		os.Exit(2)
	}
	if !strings.HasPrefix(strings.ToLower(env.Model), "claude-") {
		fatal("AGENT_MODEL=%q: suspend/resume is only wired for the Claude backend today, use a claude-* model", env.Model)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	for _, p := range []string{env.CheckpointPath, env.QuestionsPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			fatal("creating state dir for %q: %v", p, err)
		}
	}
	store, err := jsonlstore.New(env.CheckpointPath)
	if err != nil {
		fatal("checkpoint store: %v", err)
	}
	questions := &fileQuestions{path: env.QuestionsPath}
	coord, err := suspend.New(store, questions, wakeInterval)
	if err != nil {
		fatal("coordinator: %v", err)
	}

	switch os.Args[1] {
	case "ask":
		cmdAsk(ctx, coord)
	case "status":
		cmdStatus(ctx, store, questions)
	case "answer":
		if len(os.Args) < 3 {
			fatal("usage: askhuman-demo answer <text>")
		}
		cmdAnswer(questions, strings.Join(os.Args[2:], " "))
	case "resume":
		cmdResume(ctx, coord)
	case "clean":
		cmdClean(ctx, store, questions)
	default:
		fatal("unknown subcommand %q", os.Args[1])
	}
}

func cmdAsk(ctx context.Context, coord *suspend.Coordinator) {
	step("PROCESS %d — starting agent run (model %s, project %s)", os.Getpid(), env.Model, env.Project)
	agent, err := newAgent(ctx)
	if err != nil {
		fatal("agent: %v", err)
	}
	plan, err := agent.Execute(ctx, &DeployRequest{Service: "billing-api", Version: "v1.4.2"}, toolcall.EmptyTools{})
	if susp, ok := checkpoint.AsSuspension(err); ok {
		step("model called %s — agent SUSPENDED at turn %d", askHumanTool, susp.Turn)
		if err := coord.Suspend(ctx, env.Key, susp); err != nil {
			if delay, ok := workqueue.GetRequeueDelay(err); ok {
				step("checkpoint (%s) + question (%s) persisted — parked (workqueue would requeue in %s; no process holds any state now)",
					env.CheckpointPath, env.QuestionsPath, delay.Round(time.Second))
				step("next: `askhuman-demo status`, then `askhuman-demo answer <env>`")
				return
			}
			fatal("suspend: %v", err)
		}
		return
	}
	if err != nil {
		fatal("execute: %v", err)
	}
	step("agent finished without asking (unexpected for the demo prompt): %+v", plan)
}

func cmdStatus(ctx context.Context, store checkpoint.Store, questions *fileQuestions) {
	envl, _, found, err := store.Load(ctx, env.Key)
	if err != nil {
		fatal("load: %v", err)
	}
	if !found {
		step("no checkpoint for key %q — nothing is paused", env.Key)
		return
	}
	step("CHECKPOINT %s (key %q)", env.CheckpointPath, env.Key)
	pretty(map[string]any{
		"provider": envl.Provider, "model": envl.Model, "run_id": envl.RunID,
		"turn": envl.Turn, "remaining_turns": envl.RemainingTurns,
		"config_digest": envl.ConfigDigest, "trace_id": envl.TraceID,
		"deadline":           envl.Deadline,
		"pending_tool_calls": envl.PendingToolCalls,
	})
	doc, err := questions.read(env.Key)
	if err != nil {
		fatal("question: %v", err)
	}
	if doc != nil {
		step("QUESTION %s (key %q)", env.QuestionsPath, env.Key)
		pretty(doc)
	}
}

func cmdAnswer(questions *fileQuestions, text string) {
	q, err := questions.provide(env.Key, text)
	if err != nil {
		fatal("answer: %v", err)
	}
	step("human answered %q (bound to question nonce %s)", text, q.ID)
	step("next: `askhuman-demo resume`")
}

func cmdResume(ctx context.Context, coord *suspend.Coordinator) {
	step("PROCESS %d — waking key %q", os.Getpid(), env.Key)
	decision, wake, err := coord.Wake(ctx, env.Key)
	if err != nil {
		fatal("wake: %v", err)
	}
	switch decision {
	case suspend.WakeFresh:
		step("WakeFresh — no live checkpoint; a reconciler would run from scratch")
	case suspend.WakeRearm:
		step("WakeRearm — question still unanswered; nothing executed, nothing mutated, would requeue in %s", wakeInterval)
	case suspend.WakeResume:
		step("WakeResume — answer present; checkpoint CLAIMED via CAS (generation token), question consumed")
		agent, err := newAgent(ctx)
		if err != nil {
			fatal("fresh agent: %v", err)
		}
		resumer, ok := metaagent.AsResumer[*DeployRequest, *DeployPlan, toolcall.EmptyTools](agent)
		if !ok {
			fatal("agent is not resumable")
		}
		answers := make(map[string]string, len(wake.Envelope.PendingToolCalls))
		for _, pc := range wake.Envelope.PendingToolCalls {
			answers[pc.ID] = wake.Answer.Text
		}
		plan, err := resumer.Resume(ctx, *wake.Envelope, answers, toolcall.EmptyTools{})
		if err != nil {
			fatal("resume: %v", err)
		}
		step("RESUMED RUN COMPLETED: environment=%q summary=%q", plan.Environment, plan.Summary)
	}
}

func cmdClean(ctx context.Context, store checkpoint.Store, questions *fileQuestions) {
	if _, tok, found, err := store.Load(ctx, env.Key); err == nil && found {
		if err := store.Delete(ctx, env.Key, tok); err != nil {
			fatal("delete checkpoint: %v", err)
		}
	}
	m, err := questions.load()
	if err != nil {
		fatal("load questions: %v", err)
	}
	if _, ok := m[env.Key]; ok {
		delete(m, env.Key)
		if err := questions.store(m); err != nil {
			fatal("delete question: %v", err)
		}
	}
	step("cleaned key %q", env.Key)
}

func step(format string, args ...any) {
	fmt.Printf("▶ "+format+"\n", args...)
}

func pretty(v any) {
	b, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		fatal("marshal: %v", err)
	}
	fmt.Println("  " + string(b))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "✗ "+format+"\n", args...)
	os.Exit(1)
}
