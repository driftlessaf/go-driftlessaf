/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package suspend

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
)

// defaultParkDeadline bounds a park whose Coordinator has no explicit
// DefaultParkDeadline: 14 days, matching the designed answer SLA and the
// checkpoint bucket TTL (docs/factory-v2/agentic-ai-suspend-resume.md,
// storage section).
const defaultParkDeadline = 14 * 24 * time.Hour

// Coordinator drives the halt/wake lifecycle. It parks suspended envelopes in
// Store, posts and reaps human questions through Questions, and expresses the
// wait as a workqueue requeue paced by WakeInterval.
//
// Construct it with New so WakeInterval is validated > 0; the exported fields
// remain settable for callers that want to wire optional hooks, but Suspend and
// Wake defensively re-check WakeInterval so a zero value can never turn a pause
// into a hot-spin.
type Coordinator struct {
	// Store is the durable home for suspended envelopes.
	Store checkpoint.Store
	// Questions is the human transport for the pending question/answer.
	Questions QuestionStore
	// WakeInterval is the base requeue delay between wake probes. Must be > 0.
	WakeInterval time.Duration

	// Jitter, when > 0, spreads wake requeues across [WakeInterval,
	// WakeInterval+Jitter) so keys that suspended together don't stampede back
	// at once. Optional; zero means no jitter.
	Jitter time.Duration

	// DefaultParkDeadline bounds how long a park may wait for a human when the
	// suspension's envelope carries no Deadline of its own: Suspend stamps
	// Envelope.Deadline = now + DefaultParkDeadline before persisting, so
	// Wake's dead-checkpoint sweep retires every park once its answer SLA
	// lapses — a suspension can never wait forever. An explicit caller-set
	// Deadline is preserved untouched. Optional; zero means the 14-day default
	// (aligned with the answer SLA and checkpoint-bucket TTL); negative values
	// are rejected by Suspend.
	DefaultParkDeadline time.Duration

	// WithBackup, if set, is invoked after the envelope is durably Saved but
	// before the question is posted, giving callers a hook to mirror the
	// checkpoint elsewhere (e.g. a secondary store or an audit log). A non-nil
	// error aborts the suspension.
	WithBackup func(ctx context.Context, key string, env *checkpoint.Envelope) error

	// ValidateEnvelope, if set, is consulted on Wake against a parked envelope.
	// Returning an error that Is(err, checkpoint.ErrConfigDrift) causes Wake to
	// treat the checkpoint as unusable: it is deleted and WakeFresh is returned.
	ValidateEnvelope func(ctx context.Context, env *checkpoint.Envelope) error

	// now is the clock, injectable for tests. nil means time.Now.
	now func() time.Time
}

// New returns a Coordinator with WakeInterval validated > 0. Optional hooks
// (WithBackup, ValidateEnvelope, Jitter) are set on the returned struct by the
// caller. Answers are carried raw end-to-end: the executor's Resume owns
// framing (checkpoint.FramedAnswers with checkpoint.DefaultAnswerMaxBytes), so
// the Coordinator imposes no answer-size policy of its own.
func New(store checkpoint.Store, questions QuestionStore, wakeInterval time.Duration) (*Coordinator, error) {
	if store == nil {
		return nil, errors.New("suspend: nil checkpoint store")
	}
	if questions == nil {
		return nil, errors.New("suspend: nil question store")
	}
	if wakeInterval <= 0 {
		return nil, fmt.Errorf("suspend: WakeInterval must be > 0, got %v", wakeInterval)
	}
	return &Coordinator{
		Store:        store,
		Questions:    questions,
		WakeInterval: wakeInterval,
	}, nil
}

func (c *Coordinator) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Suspend halts a run: it durably parks s.Envelope under key (stamping a
// deadline of now+DefaultParkDeadline when the envelope carries none, so
// every park is bounded), runs the optional backup hook, posts a fresh-nonce
// Question, and returns a workqueue.RequeueAfterWithJitter so the reconciler
// comes back later to Wake.
//
// The returned requeue error is the success path — it is the control-flow
// signal a reconciler propagates to pause the key, not a failure. A genuine
// failure (Save/backup/Ask error) is returned as an ordinary error instead.
func (c *Coordinator) Suspend(ctx context.Context, key string, s *checkpoint.Suspension) error {
	if c.WakeInterval <= 0 {
		return fmt.Errorf("suspend: WakeInterval must be > 0, got %v", c.WakeInterval)
	}
	if c.DefaultParkDeadline < 0 {
		return fmt.Errorf("suspend: DefaultParkDeadline must be >= 0, got %v", c.DefaultParkDeadline)
	}
	if s == nil {
		return errors.New("suspend: nil suspension")
	}

	env := &s.Envelope
	// The coordinator is the only layer that knows the workqueue key, so it is
	// the envelope finalization point: stamp the key and the park deadline,
	// then validate. Park-time validation is deliberate — an unpairable or
	// unreplayable envelope must fail here, before a checkpoint is persisted
	// and a human spends time answering, not at resume.
	env.ReconcilerKey = key
	// Bound the wait: envelope validation does not require a deadline and
	// envelopeDead only fails-closed on a non-zero one, so a zero-Deadline
	// envelope would otherwise park forever. Stamp the default before Save so
	// Wake's dead-checkpoint sweep retires every park this coordinator makes
	// once its answer SLA lapses. An explicit caller-set Deadline wins.
	if env.Deadline.IsZero() {
		env.Deadline = c.clock().Add(cmp.Or(c.DefaultParkDeadline, defaultParkDeadline))
	}
	if err := env.Validate(); err != nil {
		return fmt.Errorf("suspend: envelope for %q: %w", key, err)
	}
	if err := c.Store.Save(ctx, key, env); err != nil {
		return fmt.Errorf("suspend: save envelope for %q: %w", key, err)
	}

	if c.WithBackup != nil {
		if err := c.WithBackup(ctx, key, env); err != nil {
			return fmt.Errorf("suspend: backup hook for %q: %w", key, err)
		}
	}

	nonce, err := newNonce()
	if err != nil {
		return fmt.Errorf("suspend: mint question nonce: %w", err)
	}
	if err := c.Questions.Ask(ctx, key, Question{
		ID:    nonce,
		Key:   key,
		RunID: env.RunID,
		// Prefer the suspension's own question text; when the executor did not
		// carry one, derive it from the pending ask-human call's input so the
		// posted question is never blank.
		Prompt:  cmp.Or(s.Question, checkpoint.QuestionFromPending(env.PendingToolCalls)),
		AskedAt: c.clock(),
	}); err != nil {
		return fmt.Errorf("suspend: post question for %q: %w", key, err)
	}

	return workqueue.RequeueAfterWithJitter(c.WakeInterval, c.Jitter)
}

// Wake is the tri-state re-entry probe. See WakeDecision for the three
// outcomes. On WakeResume it has already claimed the envelope (Store CAS
// delete) and consumed the question; the caller owns resuming from the returned
// WakeResult. WakeResume is never accompanied by a non-nil error: the CAS claim
// is the commit point, so a failure to consume the question afterwards is
// logged rather than returned — the claimed envelope is already gone, and an
// error here would steer err-first callers into dropping the answered run. On
// WakeFresh caused by a dead checkpoint it has already deleted the stale
// envelope and best-effort consumed its orphaned question. WakeRearm mutates
// nothing.
func (c *Coordinator) Wake(ctx context.Context, key string) (WakeDecision, *WakeResult, error) {
	env, tok, ok, err := c.Store.Load(ctx, key)
	if err != nil {
		return WakeFresh, nil, fmt.Errorf("wake: load envelope for %q: %w", key, err)
	}
	if !ok {
		// Nothing parked: never suspended, or a prior wake already claimed it.
		return WakeFresh, nil, nil
	}

	// Fail-closed on a dead checkpoint: past deadline or drifted config. Delete
	// the stale envelope (best-effort CAS) and rebuild from scratch. The
	// pending question is read before the delete so the sweep holds the nonce
	// paired with the dead envelope — a question posted by a Suspend racing in
	// after the delete carries a fresh nonce, which the nonce-bound Consume
	// below leaves untouched.
	if c.envelopeDead(ctx, env) {
		q, pending, perr := c.Questions.Pending(ctx, key)
		if derr := c.Store.Delete(ctx, key, tok); derr != nil {
			if errors.Is(derr, checkpoint.ErrTokenMismatch) {
				// A concurrent actor already claimed the envelope; it owns the
				// question cleanup.
				return WakeFresh, nil, nil
			}
			return WakeFresh, nil, fmt.Errorf("wake: delete dead envelope for %q: %w", key, derr)
		}
		// The envelope is gone, so nothing can ever resume against this pause:
		// consume the orphaned question so a human is not left answering a dead
		// run. Best-effort, mirroring the post-claim Consume contract — the
		// sweep committed at the CAS delete above.
		switch {
		case perr != nil:
			clog.WarnContext(ctx, "wake: failed to read pending question while sweeping dead checkpoint",
				"key", key, "error", perr)
		case pending:
			if cerr := c.Questions.Consume(ctx, key, q.ID); cerr != nil {
				clog.WarnContext(ctx, "wake: failed to consume question while sweeping dead checkpoint",
					"key", key, "question_id", q.ID, "error", cerr)
			}
		}
		return WakeFresh, nil, nil
	}

	ans, answered, err := c.Questions.Answer(ctx, key)
	if err != nil {
		return WakeRearm, nil, fmt.Errorf("wake: read answer for %q: %w", key, err)
	}
	if !answered {
		// Parked but no answer yet: rearm without touching state.
		return WakeRearm, nil, nil
	}

	// Answered: claim the envelope via Store CAS delete. Exactly one waker wins;
	// the loser of a duplicate-wake race sees ErrTokenMismatch and rearms.
	if derr := c.Store.Delete(ctx, key, tok); derr != nil {
		if errors.Is(derr, checkpoint.ErrTokenMismatch) {
			return WakeRearm, nil, nil
		}
		return WakeRearm, nil, fmt.Errorf("wake: claim envelope for %q: %w", key, derr)
	}

	// Won the claim: consume the question so a later Answer returns false. The
	// CAS delete above is the commit point — the envelope is gone and can never
	// be re-loaded — so a Consume failure is logged, not returned: pairing an
	// error with WakeResume would invite the idiomatic err-first caller to drop
	// the WakeResult and silently lose the answered run. The leftover question
	// is harmless: Wake only consults Questions while an envelope is parked,
	// and a superseding Suspend replaces it under a fresh nonce.
	if cerr := c.Questions.Consume(ctx, key, ans.QuestionID); cerr != nil {
		clog.WarnContext(ctx, "wake: failed to consume question after claiming envelope, resuming anyway",
			"key", key, "question_id", ans.QuestionID, "error", cerr)
	}

	return WakeResume, &WakeResult{Envelope: env, Answer: ans, Token: tok}, nil
}

// envelopeDead reports whether a parked envelope must not be resumed: its
// deadline has passed, or the optional ValidateEnvelope hook signals config
// drift. Every envelope Suspend parks carries a non-zero deadline (the
// caller's, or the stamped DefaultParkDeadline), so the IsZero guard only
// spares envelopes saved into the Store outside this coordinator.
func (c *Coordinator) envelopeDead(ctx context.Context, env *checkpoint.Envelope) bool {
	if !env.Deadline.IsZero() && c.clock().After(env.Deadline) {
		return true
	}
	if c.ValidateEnvelope != nil {
		if err := c.ValidateEnvelope(ctx, env); err != nil && errors.Is(err, checkpoint.ErrConfigDrift) {
			return true
		}
	}
	return false
}

// newNonce returns a 128-bit random hex string for use as a per-pause question
// ID.
func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
