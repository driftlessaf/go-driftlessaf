/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package suspend_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/memstore"
	"chainguard.dev/driftlessaf/agents/suspend"
	"chainguard.dev/driftlessaf/agents/suspend/memquestions"
	"chainguard.dev/driftlessaf/workqueue"
)

const testKey = "org/repo#42"

func newSuspension(reason string) *checkpoint.Suspension {
	return &checkpoint.Suspension{
		Envelope: checkpoint.Envelope{
			Version:       checkpoint.EnvelopeVersion,
			Provider:      checkpoint.ProviderAnthropic,
			Model:         "claude-fable-5",
			ReconcilerKey: testKey,
			RunID:         "run-1",
			Turn:          3,
			PendingToolCalls: []checkpoint.PendingToolCall{{
				ID:   "toolu_01ABC",
				Name: "ask_a_friend",
			}},
			// Suspend validates the envelope before persisting it, so the fixture
			// must carry everything a real executor captures: provider state, a
			// config digest for the resume-side drift gate, and a positive
			// remaining turn budget.
			ProviderState:  json.RawMessage(`{"messages":[]}`),
			ConfigDigest:   "sha256:cfg",
			RemainingTurns: 8,
		},
		Question: reason,
	}
}

func newCoord(t *testing.T, store checkpoint.Store, qs suspend.QuestionStore) *suspend.Coordinator {
	t.Helper()
	c, err := suspend.New(store, qs, time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewValidatesWakeInterval(t *testing.T) {
	if _, err := suspend.New(memstore.New(), memquestions.New(), 0); err == nil {
		t.Fatal("New: want error for zero WakeInterval, got nil")
	}
	if _, err := suspend.New(memstore.New(), memquestions.New(), -time.Second); err == nil {
		t.Fatal("New: want error for negative WakeInterval, got nil")
	}
	if _, err := suspend.New(nil, memquestions.New(), time.Minute); err == nil {
		t.Fatal("New: want error for nil store, got nil")
	}
	if _, err := suspend.New(memstore.New(), nil, time.Minute); err == nil {
		t.Fatal("New: want error for nil questions, got nil")
	}
}

func TestSuspendReturnsRequeue(t *testing.T) {
	ctx := t.Context()
	store, qs := memstore.New(), memquestions.New()
	c := newCoord(t, store, qs)

	err := c.Suspend(ctx, testKey, newSuspension("proceed?"))
	delay, ok := workqueue.GetRequeueDelay(err)
	if !ok {
		t.Fatalf("Suspend: want a requeue error, got %v", err)
	}
	if delay < time.Minute {
		t.Fatalf("Suspend: requeue delay %v < WakeInterval", delay)
	}

	// Envelope parked and question posted.
	if _, _, found, err := store.Load(ctx, testKey); err != nil || !found {
		t.Fatalf("Suspend: envelope not parked: found=%v err=%v", found, err)
	}
	if _, answered, _ := qs.Answer(ctx, testKey); answered {
		t.Fatal("Suspend: question should be unanswered immediately after Suspend")
	}
}

// TestSuspendRejectsExhaustedTurnBudget proves a final-turn suspension — the
// RemainingTurns 0 shape NewAskAFriendSuspension produces when the ask-a-friend
// call fires on the last turn — fails park-time validation inside Suspend:
// no checkpoint is persisted and no question is posted, so a human is never
// asked to answer a run that could not resume.
func TestSuspendRejectsExhaustedTurnBudget(t *testing.T) {
	ctx := t.Context()
	store, qs := memstore.New(), memquestions.New()
	c := newCoord(t, store, qs)

	s := newSuspension("proceed?")
	s.RemainingTurns = 0
	err := c.Suspend(ctx, testKey, s)
	if err == nil || isRequeue(err) {
		t.Fatalf("Suspend(exhausted budget): want a validation error, got %v", err)
	}

	// Rejected before any state was touched: nothing parked, nothing asked.
	if _, _, found, _ := store.Load(ctx, testKey); found {
		t.Fatal("Suspend(exhausted budget): envelope must not be parked")
	}
	if _, ok := qs.Provide(ctx, testKey, "too late"); ok {
		t.Fatal("Suspend(exhausted budget): no question should be pending")
	}
}

func TestSuspendBackupHook(t *testing.T) {
	ctx := t.Context()
	store, qs := memstore.New(), memquestions.New()
	c := newCoord(t, store, qs)

	var backedUp string
	c.WithBackup = func(_ context.Context, key string, env *checkpoint.Envelope) error {
		backedUp = key + "/" + env.RunID
		return nil
	}
	if err := c.Suspend(ctx, testKey, newSuspension("q")); !isRequeue(err) {
		t.Fatalf("Suspend: want requeue, got %v", err)
	}
	if backedUp != testKey+"/run-1" {
		t.Fatalf("WithBackup: got %q", backedUp)
	}
}

// TestSuspendStampsParkDeadline pins the bounded-wait guarantee at park time:
// an envelope suspended without a Deadline is stamped with
// now+DefaultParkDeadline (14 days unless configured) before it is persisted,
// while an explicit caller-set Deadline is preserved untouched.
func TestSuspendStampsParkDeadline(t *testing.T) {
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		configure time.Duration // Coordinator.DefaultParkDeadline
		deadline  time.Time     // pre-set Envelope.Deadline
		want      time.Time
	}{{
		name: "zero deadline gets the 14-day default",
		want: t0.Add(14 * 24 * time.Hour),
	}, {
		name:      "configured DefaultParkDeadline overrides the 14-day default",
		configure: 72 * time.Hour,
		want:      t0.Add(72 * time.Hour),
	}, {
		name:      "explicit caller deadline is preserved",
		configure: 72 * time.Hour,
		deadline:  t0.Add(30 * time.Minute),
		want:      t0.Add(30 * time.Minute),
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store, qs := memstore.New(), memquestions.New()
			c := newCoord(t, store, qs)
			c.DefaultParkDeadline = tc.configure
			suspend.SetClock(c, func() time.Time { return t0 })

			s := newSuspension("proceed?")
			s.Deadline = tc.deadline
			if err := c.Suspend(ctx, testKey, s); !isRequeue(err) {
				t.Fatalf("Suspend: %v", err)
			}

			env, _, found, err := store.Load(ctx, testKey)
			if err != nil || !found {
				t.Fatalf("Load: found=%v err=%v", found, err)
			}
			if !env.Deadline.Equal(tc.want) {
				t.Errorf("parked Deadline: got = %v, want = %v", env.Deadline, tc.want)
			}
		})
	}
}

// TestSuspendRejectsNegativeParkDeadline proves a negative DefaultParkDeadline
// fails validation before any state is touched — it would stamp an
// already-expired deadline, turning every park into an immediate dead sweep.
func TestSuspendRejectsNegativeParkDeadline(t *testing.T) {
	ctx := t.Context()
	store, qs := memstore.New(), memquestions.New()
	c := newCoord(t, store, qs)
	c.DefaultParkDeadline = -time.Hour

	err := c.Suspend(ctx, testKey, newSuspension("proceed?"))
	if err == nil || isRequeue(err) {
		t.Fatalf("Suspend(negative park deadline): want a validation error, got %v", err)
	}
	if _, _, found, _ := store.Load(ctx, testKey); found {
		t.Fatal("Suspend(negative park deadline): envelope must not be parked")
	}
	if _, pending, _ := qs.Pending(ctx, testKey); pending {
		t.Fatal("Suspend(negative park deadline): no question should be pending")
	}
}

// TestWakeTriState exercises the three Wake outcomes for a single pause
// instance: fresh (nothing parked), rearm (parked, unanswered), resume (parked,
// answered).
func TestWakeTriState(t *testing.T) {
	ctx := t.Context()
	store, qs := memstore.New(), memquestions.New()
	c := newCoord(t, store, qs)

	// 1. WakeFresh: nothing parked.
	if d, res, err := c.Wake(ctx, testKey); err != nil || d != suspend.WakeFresh || res != nil {
		t.Fatalf("Wake(empty): got (%v,%v,%v), want (WakeFresh,nil,nil)", d, res, err)
	}

	// Suspend to park an envelope + question.
	if err := c.Suspend(ctx, testKey, newSuspension("proceed?")); !isRequeue(err) {
		t.Fatalf("Suspend: %v", err)
	}

	// 2. WakeRearm: parked but no answer, and it must not mutate state.
	if d, res, err := c.Wake(ctx, testKey); err != nil || d != suspend.WakeRearm || res != nil {
		t.Fatalf("Wake(unanswered): got (%v,%v,%v), want (WakeRearm,nil,nil)", d, res, err)
	}
	if _, _, found, _ := store.Load(ctx, testKey); !found {
		t.Fatal("Wake(unanswered): envelope should still be parked")
	}

	// A human answers.
	if _, ok := qs.Provide(ctx, testKey, "yes, proceed"); !ok {
		t.Fatal("Provide: no pending question")
	}

	// 3. WakeResume: answered → claimed and consumed.
	d, res, err := c.Wake(ctx, testKey)
	if err != nil || d != suspend.WakeResume {
		t.Fatalf("Wake(answered): got (%v,_,%v), want WakeResume", d, err)
	}
	if res == nil || res.Envelope == nil {
		t.Fatal("Wake(answered): nil result/envelope")
	}
	if res.Answer.Text != "yes, proceed" {
		t.Fatalf("Wake(answered): answer=%q", res.Answer.Text)
	}
	if res.Envelope.RunID != "run-1" {
		t.Fatalf("Wake(answered): envelope RunID=%q", res.Envelope.RunID)
	}
	// Envelope was claimed (deleted) and question consumed.
	if _, _, found, _ := store.Load(ctx, testKey); found {
		t.Fatal("Wake(answered): envelope should be claimed/deleted")
	}
	if _, answered, _ := qs.Answer(ctx, testKey); answered {
		t.Fatal("Wake(answered): question should be consumed")
	}

	// A subsequent wake is fresh again.
	if d, _, err := c.Wake(ctx, testKey); err != nil || d != suspend.WakeFresh {
		t.Fatalf("Wake(post-claim): got (%v,%v), want WakeFresh", d, err)
	}
}

// claimRacingStore wraps a checkpoint.Store and, when armed, simulates a
// concurrent waker winning the claim race: right after a Load returns, it
// claims (CAS-deletes) the entry with the just-loaded token, so the caller's
// own Delete runs with a stale token and loses with
// checkpoint.ErrTokenMismatch. This freezes the two-wakers-load-before-either-
// deletes interleaving at the moment the winner's Delete has landed but its
// Consume has not yet run.
type claimRacingStore struct {
	checkpoint.Store
	armed bool
}

func (s *claimRacingStore) Load(ctx context.Context, key string) (*checkpoint.Envelope, checkpoint.Token, bool, error) {
	env, tok, ok, err := s.Store.Load(ctx, key)
	if err == nil && ok && s.armed {
		s.armed = false
		if derr := s.Delete(ctx, key, tok); derr != nil {
			return nil, checkpoint.Token{}, false, derr
		}
	}
	return env, tok, ok, err
}

// TestWakeDuplicateCAS proves the claim-once semantics from the loser's seat:
// a Wake whose CAS delete loses to a concurrent claimant must itself return
// (WakeRearm, nil, nil) — not an error, and not a spurious WakeResume — and
// must mutate nothing. (TestWakeTriState covers the winner's WakeResume path.)
func TestWakeDuplicateCAS(t *testing.T) {
	ctx := t.Context()
	race := &claimRacingStore{Store: memstore.New()}
	qs := memquestions.New()
	c := newCoord(t, race, qs)

	if err := c.Suspend(ctx, testKey, newSuspension("proceed?")); !isRequeue(err) {
		t.Fatalf("Suspend: %v", err)
	}
	if _, ok := qs.Provide(ctx, testKey, "go"); !ok {
		t.Fatal("Provide failed")
	}

	// Arm the race: a concurrent waker claims the envelope between this Wake's
	// Load and its CAS delete. This Wake is the loser and must rearm.
	race.armed = true
	d, res, err := c.Wake(ctx, testKey)
	if err != nil || d != suspend.WakeRearm || res != nil {
		t.Fatalf("losing Wake: got (%v,%v,%v), want (WakeRearm,nil,nil)", d, res, err)
	}

	// Losing must be harmless: the loser consumed nothing, so the answer is
	// still pending for the winner's in-flight Consume.
	if _, answered, _ := qs.Answer(ctx, testKey); !answered {
		t.Fatal("losing Wake: must not consume the question")
	}

	// Once the winner has claimed, a later probe finds nothing parked.
	if d2, _, err := c.Wake(ctx, testKey); err != nil || d2 != suspend.WakeFresh {
		t.Fatalf("post-claim Wake: got (%v,%v), want WakeFresh", d2, err)
	}
}

// faultStore wraps a checkpoint.Store with per-method error injection.
type faultStore struct {
	checkpoint.Store
	saveErr   error
	loadErr   error
	deleteErr error
}

func (s *faultStore) Save(ctx context.Context, key string, env *checkpoint.Envelope) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.Store.Save(ctx, key, env)
}

func (s *faultStore) Load(ctx context.Context, key string) (*checkpoint.Envelope, checkpoint.Token, bool, error) {
	if s.loadErr != nil {
		return nil, checkpoint.Token{}, false, s.loadErr
	}
	return s.Store.Load(ctx, key)
}

func (s *faultStore) Delete(ctx context.Context, key string, tok checkpoint.Token) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.Store.Delete(ctx, key, tok)
}

// faultQuestions wraps a memquestions.Store with per-method error injection,
// plus an Ask call counter for pinning Suspend's crash-safe ordering. The
// concrete embed keeps Provide available to tests.
type faultQuestions struct {
	*memquestions.Store
	askErr     error
	pendingErr error
	answerErr  error
	consumeErr error
	askCalls   int
}

func (q *faultQuestions) Ask(ctx context.Context, key string, question suspend.Question) error {
	q.askCalls++
	if q.askErr != nil {
		return q.askErr
	}
	return q.Store.Ask(ctx, key, question)
}

func (q *faultQuestions) Pending(ctx context.Context, key string) (suspend.Question, bool, error) {
	if q.pendingErr != nil {
		return suspend.Question{}, false, q.pendingErr
	}
	return q.Store.Pending(ctx, key)
}

func (q *faultQuestions) Answer(ctx context.Context, key string) (suspend.Answer, bool, error) {
	if q.answerErr != nil {
		return suspend.Answer{}, false, q.answerErr
	}
	return q.Store.Answer(ctx, key)
}

func (q *faultQuestions) Consume(ctx context.Context, key, questionID string) error {
	if q.consumeErr != nil {
		return q.consumeErr
	}
	return q.Store.Consume(ctx, key, questionID)
}

// TestSuspendErrorPaths drives each Suspend failure through the fault fakes
// and pins the crash-safe ordering: the question is posted last, so a Save or
// backup-hook failure must abort the suspension before a human is ever asked
// to answer a run that never parked. Every failure surfaces as an ordinary
// error, never a requeue. (Park-time validation failure is covered by
// TestSuspendRejectsExhaustedTurnBudget.)
func TestSuspendErrorPaths(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name     string
		arrange  func(c *suspend.Coordinator, store *faultStore, qs *faultQuestions)
		wantAsks int
	}{{
		name:    "save fails",
		arrange: func(_ *suspend.Coordinator, store *faultStore, _ *faultQuestions) { store.saveErr = errBoom },
	}, {
		name: "backup hook fails",
		arrange: func(c *suspend.Coordinator, _ *faultStore, _ *faultQuestions) {
			c.WithBackup = func(context.Context, string, *checkpoint.Envelope) error { return errBoom }
		},
	}, {
		name:     "ask fails",
		arrange:  func(_ *suspend.Coordinator, _ *faultStore, qs *faultQuestions) { qs.askErr = errBoom },
		wantAsks: 1,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store := &faultStore{Store: memstore.New()}
			qs := &faultQuestions{Store: memquestions.New()}
			c := newCoord(t, store, qs)
			tc.arrange(c, store, qs)

			err := c.Suspend(ctx, testKey, newSuspension("proceed?"))
			if !errors.Is(err, errBoom) {
				t.Fatalf("Suspend: got %v, want %v", err, errBoom)
			}
			if isRequeue(err) {
				t.Fatalf("Suspend: a failure must not be a requeue, got %v", err)
			}
			if got := qs.askCalls; got != tc.wantAsks {
				t.Fatalf("Ask calls: got %d, want %d", got, tc.wantAsks)
			}
		})
	}
}

// TestWakeErrorPaths drives each Wake failure through the fault fakes and pins
// the decision paired with each error, plus the invariant that a failed wake
// never loses a parked envelope: everything after a successful Load rearms (or
// stays fresh on the dead sweep) with the checkpoint still in the store, so a
// later probe can retry.
func TestWakeErrorPaths(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name    string
		arrange func(ctx context.Context, t *testing.T, c *suspend.Coordinator, store *faultStore, qs *faultQuestions)
		want    suspend.WakeDecision
		parked  bool // envelope still in the store after the failed wake
	}{{
		name: "load fails",
		arrange: func(_ context.Context, _ *testing.T, _ *suspend.Coordinator, store *faultStore, _ *faultQuestions) {
			store.loadErr = errBoom
		},
		want: suspend.WakeFresh,
	}, {
		name: "answer fails",
		arrange: func(ctx context.Context, t *testing.T, c *suspend.Coordinator, _ *faultStore, qs *faultQuestions) {
			if err := c.Suspend(ctx, testKey, newSuspension("q")); !isRequeue(err) {
				t.Fatalf("Suspend: %v", err)
			}
			qs.answerErr = errBoom
		},
		want:   suspend.WakeRearm,
		parked: true,
	}, {
		name: "claim delete fails",
		arrange: func(ctx context.Context, t *testing.T, c *suspend.Coordinator, store *faultStore, qs *faultQuestions) {
			if err := c.Suspend(ctx, testKey, newSuspension("q")); !isRequeue(err) {
				t.Fatalf("Suspend: %v", err)
			}
			if _, ok := qs.Provide(ctx, testKey, "go"); !ok {
				t.Fatal("Provide failed")
			}
			store.deleteErr = errBoom
		},
		want:   suspend.WakeRearm,
		parked: true,
	}, {
		name: "dead sweep delete fails",
		arrange: func(ctx context.Context, t *testing.T, c *suspend.Coordinator, store *faultStore, _ *faultQuestions) {
			c.ValidateEnvelope = func(context.Context, *checkpoint.Envelope) error {
				return checkpoint.ErrConfigDrift
			}
			if err := c.Suspend(ctx, testKey, newSuspension("q")); !isRequeue(err) {
				t.Fatalf("Suspend: %v", err)
			}
			store.deleteErr = errBoom
		},
		want:   suspend.WakeFresh,
		parked: true,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store := &faultStore{Store: memstore.New()}
			qs := &faultQuestions{Store: memquestions.New()}
			c := newCoord(t, store, qs)
			tc.arrange(ctx, t, c, store, qs)

			d, res, err := c.Wake(ctx, testKey)
			if !errors.Is(err, errBoom) {
				t.Fatalf("Wake: got err %v, want %v", err, errBoom)
			}
			if d != tc.want || res != nil {
				t.Fatalf("Wake: got (%v,%v), want (%v,nil)", d, res, tc.want)
			}
			// A failed wake must never lose the parked envelope.
			store.loadErr, store.deleteErr = nil, nil
			if _, _, found, err := store.Load(ctx, testKey); err != nil || found != tc.parked {
				t.Fatalf("post-failure Load: found=%v err=%v, want found=%v", found, err, tc.parked)
			}
		})
	}
}

// TestWakeConsumeFailureStillResumes proves the CAS claim is the commit point:
// a Questions.Consume failure after the claim must not surface as an error —
// Wake returns (WakeResume, result, nil), so an err-first caller can never be
// steered into dropping the claimed run (the envelope is already deleted and
// could not be re-loaded on a retry).
func TestWakeConsumeFailureStillResumes(t *testing.T) {
	ctx := t.Context()
	store := memstore.New()
	qs := &faultQuestions{Store: memquestions.New(), consumeErr: errors.New("transport down")}
	c := newCoord(t, store, qs)

	if err := c.Suspend(ctx, testKey, newSuspension("proceed?")); !isRequeue(err) {
		t.Fatalf("Suspend: %v", err)
	}
	if _, ok := qs.Provide(ctx, testKey, "go"); !ok {
		t.Fatal("Provide failed")
	}

	d, res, err := c.Wake(ctx, testKey)
	if err != nil {
		t.Fatalf("Wake: want nil error on committed resume, got %v", err)
	}
	if d != suspend.WakeResume || res == nil || res.Answer.Text != "go" {
		t.Fatalf("Wake: got (%v,%+v), want WakeResume with the answer", d, res)
	}

	// The leftover unconsumed question is harmless: nothing is parked anymore,
	// so a later Wake never consults it and starts fresh.
	if d2, _, err := c.Wake(ctx, testKey); err != nil || d2 != suspend.WakeFresh {
		t.Fatalf("post-resume Wake: got (%v,%v), want WakeFresh", d2, err)
	}
}

// TestWakeDeadlineExpired proves a past-deadline envelope is deleted and yields
// WakeFresh even though it exists.
func TestWakeDeadlineExpired(t *testing.T) {
	ctx := t.Context()
	store, qs := memstore.New(), memquestions.New()
	c := newCoord(t, store, qs)

	s := newSuspension("proceed?")
	s.Deadline = time.Now().Add(-time.Hour)
	if err := store.Save(ctx, testKey, &s.Envelope); err != nil {
		t.Fatalf("Save: %v", err)
	}

	d, res, err := c.Wake(ctx, testKey)
	if err != nil || d != suspend.WakeFresh || res != nil {
		t.Fatalf("Wake(expired): got (%v,%v,%v), want (WakeFresh,nil,nil)", d, res, err)
	}
	if _, _, found, _ := store.Load(ctx, testKey); found {
		t.Fatal("Wake(expired): stale envelope should be deleted")
	}
}

// TestWakeSweepsStampedParkDeadline proves the stamped default bounds the wait
// end-to-end: a park whose envelope carried no deadline rearms while the
// 14-day window is open, and once it lapses unanswered the dead-checkpoint
// sweep retires both the envelope and its question, yielding WakeFresh.
func TestWakeSweepsStampedParkDeadline(t *testing.T) {
	ctx := t.Context()
	store, qs := memstore.New(), memquestions.New()
	c := newCoord(t, store, qs)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	suspend.SetClock(c, func() time.Time { return now })

	if err := c.Suspend(ctx, testKey, newSuspension("proceed?")); !isRequeue(err) {
		t.Fatalf("Suspend: %v", err)
	}

	// Within the stamped window: parked and still waiting for the human.
	now = now.Add(13 * 24 * time.Hour)
	if d, res, err := c.Wake(ctx, testKey); err != nil || d != suspend.WakeRearm || res != nil {
		t.Fatalf("Wake(within window): got (%v,%v,%v), want (WakeRearm,nil,nil)", d, res, err)
	}

	// Past the window: the dead sweep bounds the park.
	now = now.Add(24*time.Hour + time.Second)
	if d, res, err := c.Wake(ctx, testKey); err != nil || d != suspend.WakeFresh || res != nil {
		t.Fatalf("Wake(past window): got (%v,%v,%v), want (WakeFresh,nil,nil)", d, res, err)
	}
	if _, _, found, _ := store.Load(ctx, testKey); found {
		t.Fatal("Wake(past window): expired envelope should be deleted")
	}
	if _, pending, err := qs.Pending(ctx, testKey); err != nil || pending {
		t.Fatalf("Wake(past window): orphaned question should be consumed, pending=%v err=%v", pending, err)
	}
}

// TestWakeConfigDrift proves the ValidateEnvelope hook deletes and yields
// fresh, and that the sweep consumes the orphaned question so a human is not
// left answering a run that no longer exists.
func TestWakeConfigDrift(t *testing.T) {
	ctx := t.Context()
	store, qs := memstore.New(), memquestions.New()
	c := newCoord(t, store, qs)
	c.ValidateEnvelope = func(_ context.Context, _ *checkpoint.Envelope) error {
		return checkpoint.ErrConfigDrift
	}

	if err := c.Suspend(ctx, testKey, newSuspension("proceed?")); !isRequeue(err) {
		t.Fatalf("Suspend: %v", err)
	}
	if _, ok := qs.Provide(ctx, testKey, "go"); !ok {
		t.Fatal("Provide failed")
	}

	d, res, err := c.Wake(ctx, testKey)
	if err != nil || d != suspend.WakeFresh || res != nil {
		t.Fatalf("Wake(drift): got (%v,%v,%v), want (WakeFresh,nil,nil)", d, res, err)
	}
	if _, _, found, _ := store.Load(ctx, testKey); found {
		t.Fatal("Wake(drift): drifted envelope should be deleted")
	}
	// The sweep retired the question along with the envelope: nothing is left
	// pending for a human to answer.
	if _, pending, err := qs.Pending(ctx, testKey); err != nil || pending {
		t.Fatalf("Wake(drift): orphaned question should be consumed, pending=%v err=%v", pending, err)
	}
}

// TestWakeDeadSweepConsumeBestEffort proves question cleanup during a dead
// sweep never blocks the sweep itself: a Pending or Consume failure is logged,
// not returned — the sweep already committed at the CAS delete, so Wake still
// reports (WakeFresh, nil, nil) with the envelope gone.
func TestWakeDeadSweepConsumeBestEffort(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name    string
		arrange func(qs *faultQuestions)
	}{{
		name:    "pending read fails",
		arrange: func(qs *faultQuestions) { qs.pendingErr = errBoom },
	}, {
		name:    "consume fails",
		arrange: func(qs *faultQuestions) { qs.consumeErr = errBoom },
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store := memstore.New()
			qs := &faultQuestions{Store: memquestions.New()}
			c := newCoord(t, store, qs)
			c.ValidateEnvelope = func(_ context.Context, _ *checkpoint.Envelope) error {
				return checkpoint.ErrConfigDrift
			}

			if err := c.Suspend(ctx, testKey, newSuspension("proceed?")); !isRequeue(err) {
				t.Fatalf("Suspend: %v", err)
			}
			tc.arrange(qs)

			d, res, err := c.Wake(ctx, testKey)
			if err != nil || d != suspend.WakeFresh || res != nil {
				t.Fatalf("Wake(dead): got (%v,%v,%v), want (WakeFresh,nil,nil)", d, res, err)
			}
			if _, _, found, _ := store.Load(ctx, testKey); found {
				t.Fatal("Wake(dead): stale envelope should be deleted despite cleanup failure")
			}
		})
	}
}

func isRequeue(err error) bool {
	_, ok := workqueue.GetRequeueDelay(err)
	return ok
}
