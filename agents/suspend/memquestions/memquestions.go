/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package memquestions

import (
	"context"
	"sync"

	"chainguard.dev/driftlessaf/agents/suspend"
)

// Store is an in-memory QuestionStore. The zero value is not usable; call New.
// It is safe for concurrent use.
type Store struct {
	mu sync.Mutex
	m  map[string]*entry
}

type entry struct {
	q      suspend.Question
	answer *suspend.Answer // nil until Provide is called for the current q
}

// New returns an empty in-memory question store.
func New() *Store {
	return &Store{m: make(map[string]*entry)}
}

var _ suspend.QuestionStore = (*Store)(nil)

// Ask posts q as the pending question for key, discarding any previous question
// and answer for that key.
func (s *Store) Ask(_ context.Context, key string, q suspend.Question) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = &entry{q: q}
	return nil
}

// Pending returns the pending question for key, if one exists.
func (s *Store) Pending(_ context.Context, key string) (suspend.Question, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok {
		return suspend.Question{}, false, nil
	}
	return e.q, true, nil
}

// Answer returns the pending question's answer for key, if one has been
// Provided and it is bound to the current question's nonce.
func (s *Store) Answer(_ context.Context, key string) (suspend.Answer, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok || e.answer == nil {
		return suspend.Answer{}, false, nil
	}
	// Nonce binding: only surface an answer bound to the current question.
	if e.answer.QuestionID != e.q.ID {
		return suspend.Answer{}, false, nil
	}
	return *e.answer, true, nil
}

// Consume clears the pending question/answer for key when questionID matches
// the current question. A mismatch is a no-op (the answer belonged to a
// superseded pause).
func (s *Store) Consume(_ context.Context, key, questionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok || e.q.ID != questionID {
		return nil
	}
	delete(s.m, key)
	return nil
}

// Provide records a human answer for key's pending question. It is the test/demo
// stand-in for a real human-transport ingress. The answer is bound to the
// current question's nonce; if no question is pending, Provide reports false.
func (s *Store) Provide(_ context.Context, key, text string) (suspend.Question, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok {
		return suspend.Question{}, false
	}
	e.answer = &suspend.Answer{
		QuestionID: e.q.ID,
		Text:       text,
		AnsweredAt: e.q.AskedAt, // deterministic for tests; real transport stamps its own
	}
	return e.q, true
}
