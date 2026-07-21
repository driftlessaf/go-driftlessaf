/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/storage"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/gcsstore"
	"chainguard.dev/driftlessaf/agents/checkpoint/jsonlstore"
	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/suspend"
	"chainguard.dev/driftlessaf/agents/suspend/githubquestions"
	"chainguard.dev/driftlessaf/agents/suspend/memquestions"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
)

// askAFriendToolName is the held-out suspend tool advertised to the model when the
// ENABLE_ASK_A_FRIEND demo path is active. The model calls it to pause the run for a
// friend answer; the executor then returns a *checkpoint.Suspension.
const askAFriendToolName = "ask_a_friend"

// askAFriendToolDescription is the friend-facing description advertised for the
// suspend tool.
const askAFriendToolDescription = "Pause the run and ask a friend operator a question. " +
	"Provide the question text; the run resumes once a friend answers."

// askAFriendCheckpointPrefix is the object-name prefix for parked envelopes
// when CHECKPOINT_BUCKET wires the durable GCS checkpoint store.
const askAFriendCheckpointPrefix = "github-pr-autofix"

// errResumeFailed marks a non-suspension failure from resuming a CLAIMED run
// (the wake already CAS-deleted the envelope and consumed the answer). The
// reconcile must return this error to the workqueue rather than writing a
// terminal failure status: terminal-failing would end the key's processing
// with the parked conversation and the friend's answer silently dropped, while
// a workqueue retry re-reconciles and re-runs from scratch.
var errResumeFailed = errors.New("ask-a-friend: resuming claimed run failed")

// prAgentFactory constructs a fresh meta-agent. The ask-a-friend resume path builds
// a NEW agent on every wake rather than reusing a long-lived one: a meta-agent
// binds its executor exactly once at construction, so resuming against a stale
// captured agent would replay against the wrong live state. Constructing per wake
// keeps the executor config aligned with the parked envelope's ConfigDigest.
type prAgentFactory func(ctx context.Context) (metaagent.Agent[*PRContext, *PRFixResult, PRTools], error)

// askAFriendReconciler carries the suspend/resume lifecycle wiring for the
// ENABLE_ASK_A_FRIEND demo. With CHECKPOINT_BUCKET set, the run is durable and
// the friend transport is GitHub itself: envelopes park in GCS (gcsstore) and
// the question is posted as a PR comment that a collaborator answers with
// "/answer <text>" (githubquestions). Without it, demo-grade stand-ins — a
// local-file jsonlstore and an in-memory memquestions — that do not survive
// a restart.
type askAFriendReconciler struct {
	coord    *suspend.Coordinator
	newAgent prAgentFactory
}

// newAskAFriendReconciler wires a checkpoint store and question store into a
// suspend.Coordinator, and captures a factory that mints a fresh
// suspend-enabled meta-agent per wake. CHECKPOINT_BUCKET selects the durable
// path (GCS envelopes + PR-comment questions via clients); otherwise the
// local-file/in-memory demo pair is used. It requires a claude-* model:
// suspend/resume is only wired for the Claude backend today (the other
// metaagent backends reject a set SuspendToolName at construction), so
// failing fast at startup beats erroring on the first reconcile.
func newAskAFriendReconciler(ctx context.Context, cfg *config, clients *githubreconciler.ClientCache) (*askAFriendReconciler, error) {
	if !strings.HasPrefix(strings.ToLower(cfg.Model), "claude-") {
		return nil, fmt.Errorf("ENABLE_ASK_A_FRIEND requires a claude-* model (suspend/resume is only wired for the Claude backend today), got AGENT_MODEL=%q", cfg.Model)
	}
	var store checkpoint.Store
	var questions suspend.QuestionStore
	if cfg.AskAFriendBucket != "" {
		if clients == nil {
			return nil, errors.New("CHECKPOINT_BUCKET requires a GitHub client cache: questions are posted as PR comments")
		}
		client, err := storage.NewClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("creating storage client: %w", err)
		}
		// IdentitySealer stores envelopes as readable JSON — right for a demo
		// bucket where inspecting the parked state is the point; production
		// seals with a KMS-envelope Sealer (see the gcsstore package doc).
		store = gcsstore.New(askAFriendCheckpointPrefix, client.Bucket(cfg.AskAFriendBucket), gcsstore.IdentitySealer{})
		questions = githubquestions.New(clients)
	} else {
		js, err := jsonlstore.New(cfg.AskAFriendSnapshotPath)
		if err != nil {
			return nil, fmt.Errorf("opening checkpoint store: %w", err)
		}
		store = js
		questions = memquestions.New()
	}
	coord, err := suspend.New(store, questions, cfg.AskAFriendWakeInterval)
	if err != nil {
		return nil, fmt.Errorf("creating suspend coordinator: %w", err)
	}
	// Spread wake requeues so keys that suspended together don't re-poll the
	// stores in a synchronized burst every interval for as long as they stay
	// parked.
	coord.Jitter = cfg.AskAFriendWakeInterval / 2
	return &askAFriendReconciler{
		coord:    coord,
		newAgent: newAskAFriendAgentFactory(cfg),
	}, nil
}

// newAskAFriendAgentFactory returns a factory that builds a fresh meta-agent with
// the ask-a-friend suspend tool advertised. The suspend tool is opt-in via the
// additive metaagent.Config fields; the agent is otherwise identical to the
// plain autofix agent.
func newAskAFriendAgentFactory(cfg *config) prAgentFactory {
	return func(ctx context.Context) (metaagent.Agent[*PRContext, *PRFixResult, PRTools], error) {
		return metaagent.New[*PRContext, *PRFixResult, PRTools]( //nolint:infertypeargs // Req cannot be inferred from Config
			ctx, cfg.GCPProjectID, cfg.GCPRegion, cfg.Model,
			metaagent.Config[*PRFixResult, PRTools]{
				SystemInstructions:     systemInstructions,
				UserPrompt:             userPrompt,
				Tools:                  NewPRToolsProvider(),
				SuspendToolName:        askAFriendToolName,
				SuspendToolDescription: askAFriendToolDescription,
			},
		)
	}
}

// run drives one reconcile of the ask-a-friend lifecycle for key. It is the demo's
// tri-state entrypoint. beginFresh owns the caller's fix-attempt accounting
// (the MaxFixAttempts gate plus the in_progress attempt-status write) and is
// invoked exactly once, only when the wake decision is WakeFresh, immediately
// before the fresh execution — keeping it out of the rearm and resume branches
// is what keeps the attempt accounting honest across a park:
//
//   - WakeFresh: gate and record the attempt via beginFresh, then run the
//     agent from scratch. If the model calls the ask-a-friend tool, Execute
//     returns a *checkpoint.Suspension; the run is parked via
//     Coordinator.Suspend, which returns a workqueue requeue (the pause
//     signal). No result is produced and no fix attempt is "spent" beyond the
//     one fresh run.
//   - WakeRearm: a checkpoint is parked but not yet actionable (unanswered, or
//     a concurrent waker already claimed it). Return a cheap RequeueAfter with
//     NO fresh execution, NO status write, and NO fix attempt burned. This is
//     the poll wake that must never count against MAX_FIX_ATTEMPTS — a parked
//     run would otherwise be marked failed by its own polls before a friend
//     could answer.
//   - WakeResume: the question was answered and the envelope claimed (the CAS
//     delete is the commit point — Wake never pairs WakeResume with an error,
//     so the claimed run is never dropped). Build a FRESH agent, obtain its
//     Resumer capability, and resume — pairing the raw answer to the persisted
//     pending tool-call IDs (from Envelope.PendingToolCalls, never re-derived
//     from the tool name); the executor's Resume owns framing. The resume
//     continues the attempt its fresh run already recorded, so beginFresh is
//     not consulted and the attempts gate cannot strand a claimed envelope.
//     A resume that suspends again (a multi-question conversation) re-parks
//     via Coordinator.Suspend, exactly like the fresh branch.
func (r *askAFriendReconciler) run(ctx context.Context, key string, req *PRContext, cb PRTools, beginFresh func(context.Context) error) (*PRFixResult, error) {
	decision, res, err := r.coord.Wake(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("ask-a-friend wake: %w", err)
	}

	switch decision {
	case suspend.WakeRearm:
		// Parked and waiting on the friend: requeue cheaply, touch nothing, and burn
		// no fix attempt. This is the poll wake that must never re-execute.
		clog.InfoContextf(ctx, "ask-a-friend: %s still awaiting an answer, rearming", key)
		return nil, workqueue.RequeueAfterWithJitter(r.coord.WakeInterval, r.coord.Jitter)

	case suspend.WakeResume:
		clog.InfoContextf(ctx, "ask-a-friend: %s answered, resuming", key)
		agent, err := r.newAgent(ctx)
		if err != nil {
			return nil, fmt.Errorf("ask-a-friend: building resume agent: %w", err)
		}
		resumer, ok := metaagent.AsResumer[*PRContext, *PRFixResult, PRTools](agent)
		if !ok {
			return nil, errors.New("ask-a-friend: agent backend does not support resume")
		}
		answers := answersForPending(res.Envelope, res.Answer)
		result, err := resumer.Resume(ctx, *res.Envelope, answers, cb)
		// A resumed run drives the same turn loop as a fresh one, so it can
		// call the ask-a-friend tool again. Re-park exactly like the fresh
		// branch does — otherwise the new suspension would fall through to
		// the generic error handler as a terminal failure, orphaning the
		// re-parked conversation.
		if s, ok := checkpoint.AsSuspension(err); ok {
			clog.InfoContextf(ctx, "ask-a-friend: %s suspended again at turn %d, re-parking", key, s.Turn)
			return nil, r.coord.Suspend(ctx, key, s)
		}
		if err != nil {
			// The wake already claimed the envelope and consumed the answer;
			// a terminal-failure status here would silently drop both. Mark
			// the error so the caller surfaces it to the workqueue instead:
			// the retry finds no checkpoint (WakeFresh) and re-runs from
			// scratch — work lost, never a stranded claimed run.
			return nil, errors.Join(errResumeFailed, err)
		}
		return result, nil

	default: // WakeFresh
		if err := beginFresh(ctx); err != nil {
			return nil, err
		}
		agent, err := r.newAgent(ctx)
		if err != nil {
			return nil, fmt.Errorf("ask-a-friend: building fresh agent: %w", err)
		}
		result, err := agent.Execute(ctx, req, cb)
		if s, ok := checkpoint.AsSuspension(err); ok {
			// The model asked a friend. Park the checkpoint and post the question;
			// Suspend returns a workqueue requeue that pauses the key.
			clog.InfoContextf(ctx, "ask-a-friend: %s suspended at turn %d, parking", key, s.Turn)
			return nil, r.coord.Suspend(ctx, key, s)
		}
		return result, err
	}
}

// answersForPending maps each pending tool-call ID to the raw friend answer
// text (framing is the executor's job on resume). The IDs come from
// Envelope.PendingToolCalls — the provider's own tool_use / function-call
// identifiers persisted at suspend time — so an answer is never paired by
// re-deriving an ID from the tool name (which would break the moment a turn
// issues two calls to one tool).
func answersForPending(env *checkpoint.Envelope, ans suspend.Answer) map[string]string {
	m := make(map[string]string, len(env.PendingToolCalls))
	for _, pc := range env.PendingToolCalls {
		m[pc.ID] = ans.Text
	}
	return m
}
