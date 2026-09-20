/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package systemone

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Answer is one typed answer. The three implementations are [NoulAnswer],
// [ChoiceAnswer], and [ScoreAnswer], matching the question kinds; switch on
// the concrete type or on Kind to read the value.
type Answer interface {
	// Kind returns the wire kind: "noul", "choice", or "score".
	Kind() string
	answer()
}

// NoulAnswer answers a [Noul] question.
type NoulAnswer struct {
	// Probability is the calibrated probability, in [0, 1], that the answer
	// is yes.
	Probability float64
}

var _ Answer = (*NoulAnswer)(nil)

// Kind returns "noul".
func (NoulAnswer) Kind() string { return kindNoul }
func (NoulAnswer) answer()      {}

// ChoiceAnswer answers a [Choice] question.
type ChoiceAnswer struct {
	// Choice is the selected label, always one of the question's options.
	Choice string
	// Probabilities holds one probability per option label.
	Probabilities map[string]float64
	// Confidence is the model's overall confidence in the selection, in
	// [0, 1].
	Confidence float64
}

var _ Answer = (*ChoiceAnswer)(nil)

// Kind returns "choice".
func (ChoiceAnswer) Kind() string { return kindChoice }
func (ChoiceAnswer) answer()      {}

// ScoreAnswer answers a [Score] question.
type ScoreAnswer struct {
	// Score is the probability-weighted position on the rubric, in
	// [0, len(levels)-1].
	Score float64
	// Legend maps each level key used in Probabilities to the level's
	// description as the API rendered it.
	Legend map[string]string
	// Probabilities holds one probability per rubric level, keyed as in
	// Legend.
	Probabilities map[string]float64
	// Confidence is the model's overall confidence in the rating, in [0, 1].
	Confidence float64
}

var _ Answer = (*ScoreAnswer)(nil)

// Kind returns "score".
func (ScoreAnswer) Kind() string { return kindScore }
func (ScoreAnswer) answer()      {}

// Level returns the most probable rubric level key and its probability. Ties
// resolve to the lexically smallest key so the result is deterministic. It
// returns "" and 0 when there are no probabilities.
func (a ScoreAnswer) Level() (string, float64) {
	return mostProbable(a.Probabilities)
}

func mostProbable(probabilities map[string]float64) (string, float64) {
	best, bestP := "", math.Inf(-1)
	for key, p := range probabilities {
		if p > bestP || (p == bestP && key < best) {
			best, bestP = key, p
		}
	}
	if best == "" {
		return "", 0
	}
	return best, bestP
}

// wireAnswer is the union of every answer kind's response fields.
type wireAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Legend        map[string]string  `json:"legend"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

// normalizeQuestion returns the value form of a question. Pointer forms
// satisfy Question through Go's method-set promotion and marshal correctly,
// so they are accepted here rather than surprising the caller at decode time;
// a nil pointer or a foreign implementation is rejected.
func normalizeQuestion(question Question) (Question, error) {
	switch q := question.(type) {
	case Noul, Choice, Score:
		return q, nil
	case *Noul:
		if q == nil {
			return nil, errors.New("question is a nil *Noul")
		}
		return *q, nil
	case *Choice:
		if q == nil {
			return nil, errors.New("question is a nil *Choice")
		}
		return *q, nil
	case *Score:
		if q == nil {
			return nil, errors.New("question is a nil *Score")
		}
		return *q, nil
	case nil:
		return nil, errors.New("question is nil")
	default:
		return nil, fmt.Errorf("unsupported question type %T", question)
	}
}

// decodeAnswer decodes one answer and checks it against the question that
// produced it. Every failure wraps ErrResponseValidation. The checks are
// deliberately strict: a distribution must cover exactly the declared options
// or rubric levels and sum to one, so a consumer never reads a missing entry
// as zero or a foreign key as a level.
func decodeAnswer(id string, raw json.RawMessage, question Question) (Answer, error) {
	question, err := normalizeQuestion(question)
	if err != nil {
		return nil, validationError("answer %q: %w", id, err)
	}
	var w wireAnswer
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, validationError("answer %q: %w", id, err)
	}
	if w.Type != question.Kind() {
		return nil, validationError("answer %q: kind %q, want %q", id, w.Type, question.Kind())
	}
	switch q := question.(type) {
	case Noul:
		if err := checkUnitInterval("noul", w.Noul); err != nil {
			return nil, validationError("answer %q: %w", id, err)
		}
		return NoulAnswer{Probability: *w.Noul}, nil
	case Choice:
		if _, ok := q.Options[w.Choice]; !ok {
			return nil, validationError("answer %q: choice %q is not one of the question's options", id, w.Choice)
		}
		if err := checkDistribution(w.Probabilities, len(q.Options), func(label string) bool {
			_, ok := q.Options[label]
			return ok
		}); err != nil {
			return nil, validationError("answer %q: %w", id, err)
		}
		if err := checkUnitInterval("confidence", w.Confidence); err != nil {
			return nil, validationError("answer %q: %w", id, err)
		}
		return ChoiceAnswer{Choice: w.Choice, Probabilities: w.Probabilities, Confidence: *w.Confidence}, nil
	case Score:
		top := len(q.Levels) - 1
		if w.Score == nil || math.IsNaN(*w.Score) || *w.Score < 0 || *w.Score > float64(top) {
			return nil, validationError("answer %q: score %v is outside [0, %d]", id, deref(w.Score), top)
		}
		if err := checkDistribution(w.Probabilities, len(q.Levels), func(key string) bool {
			return isLevelKey(key, len(q.Levels))
		}); err != nil {
			return nil, validationError("answer %q: %w", id, err)
		}
		if len(w.Legend) != len(q.Levels) {
			return nil, validationError("answer %q: legend has %d entries, want %d (one per level)", id, len(w.Legend), len(q.Levels))
		}
		for key := range w.Probabilities {
			if _, ok := w.Legend[key]; !ok {
				return nil, validationError("answer %q: legend has no entry for level %q", id, key)
			}
		}
		if err := checkUnitInterval("confidence", w.Confidence); err != nil {
			return nil, validationError("answer %q: %w", id, err)
		}
		return ScoreAnswer{Score: *w.Score, Legend: w.Legend, Probabilities: w.Probabilities, Confidence: *w.Confidence}, nil
	default:
		return nil, validationError("answer %q: unsupported question type %T", id, question)
	}
}

// isLevelKey reports whether key is the decimal index of one of n rubric
// levels, as the API keys Score legends and probabilities ("0" .. "n-1").
func isLevelKey(key string, n int) bool {
	i, err := strconv.Atoi(key)
	return err == nil && i >= 0 && i < n && strconv.Itoa(i) == key
}

// checkDistribution requires probabilities to cover exactly the n declared
// entries (each key accepted by declared, no key twice by construction of a
// map), every value in [0, 1], and a total of one within rounding tolerance.
func checkDistribution(probabilities map[string]float64, n int, declared func(string) bool) error {
	if len(probabilities) != n {
		return fmt.Errorf("got %d probabilities, want %d (one per declared entry)", len(probabilities), n)
	}
	var sum float64
	for key, p := range probabilities {
		if !declared(key) {
			return fmt.Errorf("probability for undeclared entry %q", key)
		}
		if err := checkUnitInterval("probability", &p); err != nil {
			return fmt.Errorf("entry %q: %w", key, err)
		}
		sum += p
	}
	if tolerance := distributionTolerance(n); math.Abs(sum-1) > tolerance {
		return fmt.Errorf("probabilities sum to %.4f, want 1 within %.4f", sum, tolerance)
	}
	return nil
}

// distributionTolerance is the slack allowed on a distribution's total. The
// API rounds each probability independently, so the permitted drift grows
// with the number of entries.
func distributionTolerance(n int) float64 {
	return 0.01 + 0.001*float64(n)
}

func checkUnitInterval(name string, v *float64) error {
	if v == nil {
		return fmt.Errorf("%s is missing", name)
	}
	if math.IsNaN(*v) || *v < 0 || *v > 1 {
		return fmt.Errorf("%s %v is outside [0, 1]", name, *v)
	}
	return nil
}

func deref(v *float64) any {
	if v == nil {
		return "<missing>"
	}
	return *v
}

func validationError(format string, args ...any) error {
	return fmt.Errorf("%w: %w", ErrResponseValidation, fmt.Errorf(format, args...))
}
