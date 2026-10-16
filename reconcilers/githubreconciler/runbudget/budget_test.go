/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package runbudget

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestBudgetValidate(t *testing.T) {
	tests := []struct {
		name    string
		budget  Budget
		wantErr bool
	}{
		{name: "valid", budget: Budget{Total: time.Hour, Reserve: 8 * time.Minute}},
		{name: "zero total", budget: Budget{Total: 0, Reserve: time.Minute}, wantErr: true},
		{name: "zero reserve", budget: Budget{Total: time.Hour, Reserve: 0}, wantErr: true},
		{name: "negative reserve", budget: Budget{Total: time.Hour, Reserve: -time.Minute}, wantErr: true},
		{name: "reserve equals total", budget: Budget{Total: time.Hour, Reserve: time.Hour}, wantErr: true},
		{name: "reserve exceeds total", budget: Budget{Total: time.Minute, Reserve: time.Hour}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.budget.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate: got err = %v, want error = %t", err, tc.wantErr)
			}
		})
	}
}

// TestFromSeconds proves the constructor bots feed from their environment: a
// zero total opts out, and a misconfigured pair fails at startup rather than
// producing a budget whose agent phase never runs or never ends.
func TestFromSeconds(t *testing.T) {
	tests := []struct {
		name           string
		total, reserve int
		want           Budget
		wantErr        bool
	}{
		{name: "an hour-class budget", total: 3300, reserve: 480, want: Budget{Total: 55 * time.Minute, Reserve: 8 * time.Minute}},
		{name: "zero total disables the budget", total: 0, reserve: 480},
		{name: "negative total is rejected", total: -1, reserve: 480, wantErr: true},
		{name: "zero reserve is rejected", total: 3300, reserve: 0, wantErr: true},
		{name: "reserve equal to total is rejected", total: 480, reserve: 480, wantErr: true},
		{name: "reserve above total is rejected", total: 100, reserve: 480, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromSeconds(tc.total, tc.reserve)
			if (err != nil) != tc.wantErr {
				t.Fatalf("FromSeconds: got err = %v, want error = %t", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("FromSeconds: got = %+v, want = %+v", got, tc.want)
			}
		})
	}
}

func TestStartRecordsBudget(t *testing.T) {
	start := time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)
	b := Budget{Total: time.Hour, Reserve: 8 * time.Minute}

	t.Run("recorded", func(t *testing.T) {
		ctx := Start(t.Context(), b, start)
		got, gotStart, ok := Started(ctx)
		if !ok {
			t.Fatal("Started: got ok = false, want true")
		}
		if got != b {
			t.Errorf("Started budget: got = %+v, want = %+v", got, b)
		}
		if !gotStart.Equal(start) {
			t.Errorf("Started start: got = %v, want = %v", gotStart, start)
		}
	})

	t.Run("absent without Start", func(t *testing.T) {
		if _, _, ok := Started(t.Context()); ok {
			t.Error("Started on a plain context: got ok = true, want false")
		}
	})

	t.Run("zero budget records nothing", func(t *testing.T) {
		ctx := Start(t.Context(), Budget{}, start)
		if _, _, ok := Started(ctx); ok {
			t.Error("Started after a zero budget: got ok = true, want false")
		}
	})
}

func TestAgentContext(t *testing.T) {
	start := time.Now()
	b := Budget{Total: time.Hour, Reserve: 8 * time.Minute}

	t.Run("carries the agent deadline", func(t *testing.T) {
		ctx := Start(t.Context(), b, start)
		agent, cancel := AgentContext(ctx)
		defer cancel()
		got, ok := agent.Deadline()
		if !ok {
			t.Fatal("agent context has no deadline")
		}
		if want := b.agentDeadline(start); !got.Equal(want) {
			t.Errorf("agent deadline: got = %v, want = %v", got, want)
		}
	})

	t.Run("parent cancellation propagates", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(Start(t.Context(), b, start))
		agent, cancel := AgentContext(parent)
		defer cancel()
		cancelParent()
		<-agent.Done()
		if !errors.Is(agent.Err(), context.Canceled) {
			t.Errorf("agent.Err after parent cancel: got = %v, want = %v", agent.Err(), context.Canceled)
		}
	})

	t.Run("no budget leaves the deadline alone", func(t *testing.T) {
		agent, cancel := AgentContext(t.Context())
		defer cancel()
		if _, ok := agent.Deadline(); ok {
			t.Error("agent context without a budget has a deadline, want none")
		}
	})
}

func TestPublicationContext(t *testing.T) {
	start := time.Now()
	b := Budget{Total: time.Hour, Reserve: 8 * time.Minute}

	t.Run("detached from parent cancellation and bounded", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(Start(t.Context(), b, start))
		pub, cancel := PublicationContext(parent)
		defer cancel()
		cancelParent()
		if pub.Err() != nil {
			t.Errorf("publication.Err after parent cancel: got = %v, want nil", pub.Err())
		}
		got, ok := pub.Deadline()
		if !ok {
			t.Fatal("publication context has no deadline")
		}
		if want := b.publicationDeadline(start); !got.Equal(want) {
			t.Errorf("publication deadline: got = %v, want = %v", got, want)
		}
	})

	t.Run("short bound when the request already ended", func(t *testing.T) {
		request, cancelRequest := context.WithCancel(t.Context())
		parent := Start(request, b, start)
		cancelRequest()
		before := time.Now()
		pub, cancel := PublicationContext(parent)
		after := time.Now()
		defer cancel()
		if pub.Err() != nil {
			t.Errorf("publication.Err on an ended request: got = %v, want nil", pub.Err())
		}
		got, ok := pub.Deadline()
		if !ok {
			t.Fatal("publication context has no deadline")
		}
		if got.Before(before.Add(InterruptionBound)) || got.After(after.Add(InterruptionBound)) {
			t.Errorf("publication deadline on an ended request: got = %v, want %v after a time in [%v, %v]", got, InterruptionBound, before, after)
		}
		if !got.Before(b.publicationDeadline(start)) {
			t.Errorf("interruption bound %v is not before the publication deadline %v", got, b.publicationDeadline(start))
		}
	})

	// clonemanager's commit and push and changemanager's pull request update
	// each derive a publication context, from different points of the context
	// tree. After an interruption the platform grants only a short grace
	// period, so the steps share one bound rather than each re-arming it.
	t.Run("publication steps after an interruption share one bound", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			type key struct{}
			request, cancelRequest := context.WithCancel(t.Context())
			parent := Start(request, b, time.Now())

			// The commit and push started while the request was live, so it
			// is bounded by the budget alone.
			push, cancelPush := PublicationContext(context.WithValue(parent, key{}, "push"))
			defer cancelPush()
			if got, _ := push.Deadline(); time.Until(got) <= InterruptionBound {
				t.Errorf("publication deadline on a live request: got = %v, want the budget's publication deadline", got)
			}

			cancelRequest()
			interrupted := time.Now()
			first, cancelFirst := PublicationContext(parent)
			defer cancelFirst()
			time.Sleep(InterruptionBound / 2)
			second, cancelSecond := PublicationContext(context.WithValue(parent, key{}, "upsert"))
			defer cancelSecond()

			want := interrupted.Add(InterruptionBound)
			for name, pub := range map[string]context.Context{"first": first, "second": second} {
				if got, _ := pub.Deadline(); !got.Equal(want) {
					t.Errorf("%s publication deadline after the interruption: got = %v, want = %v", name, got, want)
				}
			}
		})
	})

	// A caller can hand publication a context that ended for reasons of its
	// own, such as the agent context at the agent deadline. The request is
	// still live, so publication keeps its whole reserve, and a later real
	// interruption is bounded from when it happens.
	t.Run("an ended context below a live request is not an interruption", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			request, cancelRequest := context.WithCancel(t.Context())
			defer cancelRequest()
			runStart := time.Now()
			parent := Start(request, b, runStart)
			agent, cancelAgent := AgentContext(parent)
			defer cancelAgent()
			<-agent.Done()

			pub, cancel := PublicationContext(agent)
			defer cancel()
			if pub.Err() != nil {
				t.Errorf("publication.Err after the agent deadline: got = %v, want nil", pub.Err())
			}
			if got, want := deadlineOf(t, pub), b.publicationDeadline(runStart); !got.Equal(want) {
				t.Errorf("publication deadline after the agent deadline: got = %v, want = %v", got, want)
			}

			time.Sleep(time.Minute)
			cancelRequest()
			interrupted := time.Now()
			later, cancelLater := PublicationContext(parent)
			defer cancelLater()
			if got, want := deadlineOf(t, later), interrupted.Add(InterruptionBound); !got.Equal(want) {
				t.Errorf("publication deadline after a later interruption: got = %v, want = %v", got, want)
			}
		})
	})

	t.Run("no budget returns the request context", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(t.Context())
		pub, cancel := PublicationContext(parent)
		defer cancel()
		cancelParent()
		if !errors.Is(pub.Err(), context.Canceled) {
			t.Errorf("publication.Err without a budget: got = %v, want %v", pub.Err(), context.Canceled)
		}
	})

	t.Run("carries the parent's values", func(t *testing.T) {
		type key struct{}
		parent := context.WithValue(Start(t.Context(), b, start), key{}, "value")
		pub, cancel := PublicationContext(parent)
		defer cancel()
		if got := pub.Value(key{}); got != "value" {
			t.Errorf("publication.Value: got = %v, want %q", got, "value")
		}
	})
}

func TestClassify(t *testing.T) {
	agentErr := errors.New("agent failed")

	type ctxs struct {
		parent context.Context
		agent  context.Context
	}
	tests := []struct {
		name string
		make func(t *testing.T) ctxs
		err  error
		want End
	}{
		{
			name: "success",
			make: func(t *testing.T) ctxs {
				agent, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				return ctxs{parent: t.Context(), agent: agent}
			},
			want: Finished,
		},
		{
			name: "agent error with live contexts",
			make: func(t *testing.T) ctxs {
				agent, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				return ctxs{parent: t.Context(), agent: agent}
			},
			err:  agentErr,
			want: Finished,
		},
		{
			name: "success even when the agent context has since ended",
			make: func(t *testing.T) ctxs {
				agent, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctxs{parent: t.Context(), agent: agent}
			},
			want: Finished,
		},
		{
			name: "agent deadline with a live request",
			make: func(t *testing.T) ctxs {
				agent, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctxs{parent: t.Context(), agent: agent}
			},
			err:  agentErr,
			want: DeadlineReached,
		},
		{
			name: "request canceled first",
			make: func(t *testing.T) ctxs {
				parent, cancelParent := context.WithCancel(t.Context())
				agent, cancel := context.WithDeadline(parent, time.Now().Add(time.Hour))
				t.Cleanup(cancel)
				cancelParent()
				return ctxs{parent: parent, agent: agent}
			},
			err:  agentErr,
			want: Interrupted,
		},
		{
			name: "request deadline counts as interruption",
			make: func(t *testing.T) ctxs {
				parent, cancelParent := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				t.Cleanup(cancelParent)
				agent, cancel := context.WithDeadline(parent, time.Now().Add(time.Hour))
				t.Cleanup(cancel)
				return ctxs{parent: parent, agent: agent}
			},
			err:  agentErr,
			want: Interrupted,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.make(t)
			if got := Classify(c.parent, c.agent, tc.err); got != tc.want {
				t.Errorf("Classify: got = %v, want = %v", got, tc.want)
			}
		})
	}
}

func TestEndString(t *testing.T) {
	tests := []struct {
		end  End
		want string
	}{
		{Finished, "finished"},
		{DeadlineReached, "deadline"},
		{Interrupted, "interrupted"},
		{End(42), "end(42)"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.end.String(); got != tc.want {
				t.Errorf("String: got = %q, want = %q", got, tc.want)
			}
		})
	}
}

// deadlineOf returns ctx's deadline, failing the test when it has none.
func deadlineOf(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	got, ok := ctx.Deadline()
	if !ok {
		t.Fatal("context has no deadline")
	}
	return got
}
