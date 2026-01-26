/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"context"
	"fmt"
	"sync"

	"chainguard.dev/driftlessaf/agents/judge"
)

// mockObserver implements evals.Observer for testing with full functionality
type mockObserver struct {
	mu       sync.Mutex
	failures []string
	logs     []string
	grades   []gradeRecord
	count    int64
}

type gradeRecord struct {
	score     float64
	reasoning string
}

func (m *mockObserver) Fail(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures = append(m.failures, msg)
	m.logs = append(m.logs, msg)
}

func (m *mockObserver) Log(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, msg)
}

func (m *mockObserver) Grade(score float64, reasoning string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grades = append(m.grades, gradeRecord{score: score, reasoning: reasoning})
	m.logs = append(m.logs, fmt.Sprintf("Grade: %.2f - %s", score, reasoning))
}

func (m *mockObserver) Increment() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.count++
}

func (m *mockObserver) Total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

// getFailures returns a copy of failures for thread-safe testing
func (m *mockObserver) getFailures() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	failures := make([]string, len(m.failures))
	copy(failures, m.failures)
	return failures
}

// getLogs returns a copy of logs for thread-safe testing
func (m *mockObserver) getLogs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	logs := make([]string, len(m.logs))
	copy(logs, m.logs)
	return logs
}

// getGrades returns a copy of grades for thread-safe testing
func (m *mockObserver) getGrades() []gradeRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	grades := make([]gradeRecord, len(m.grades))
	copy(grades, m.grades)
	return grades
}

// mockJudge returns predetermined judgments for testing
type mockJudge struct {
	judgment *judge.Judgement
	err      error
}

func (m *mockJudge) Judge(ctx context.Context, request *judge.Request) (*judge.Judgement, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.judgment, nil
}
