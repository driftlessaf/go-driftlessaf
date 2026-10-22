/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"cloud.google.com/go/compute/metadata"
	"github.com/google/go-cmp/cmp"
)

func TestOnGCP(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		service        string
		metadataHost   string
		dmiProductName string
		want           bool
	}{
		{name: "Cloud Run", service: "reviewbot", want: true},
		{name: "explicit metadata host", metadataHost: "127.0.0.1:8080", want: true},
		{name: "GCE VM", dmiProductName: "Google Compute Engine", want: true},
		{name: "older GCE VM", dmiProductName: "Google", want: true},
		{name: "other cloud", dmiProductName: "Virtual Machine", want: false},
		{name: "laptop", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := onGCP(tc.service, tc.metadataHost, tc.dmiProductName); got != tc.want {
				t.Errorf("onGCP: got = %t, want = %t", got, tc.want)
			}
		})
	}
}

// fakeMetadata answers metadata suffixes from a fixed table.
type fakeMetadata map[string]string

func (m fakeMetadata) get(_ context.Context, suffix string) (string, error) {
	if v, ok := m[suffix]; ok {
		return v, nil
	}
	return "", errors.New("not defined")
}

func TestLookupRegion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		metadata fakeMetadata
		want     string
		wantErr  bool
	}{
		{
			name:     "Cloud Run region",
			metadata: fakeMetadata{"instance/region": "projects/123456/regions/us-central1"},
			want:     "us-central1",
		},
		{
			name:     "region wins over zone",
			metadata: fakeMetadata{"instance/region": "projects/1/regions/us-east5", "instance/zone": "projects/1/zones/europe-west4-b"},
			want:     "us-east5",
		},
		{
			name:     "GCE zone",
			metadata: fakeMetadata{"instance/zone": "projects/123456/zones/us-east4-c\n"},
			want:     "us-east4",
		},
		{
			name:     "zone without a suffix",
			metadata: fakeMetadata{"instance/zone": "projects/1/zones/local"},
			wantErr:  true,
		},
		{name: "not available", metadata: fakeMetadata{}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := lookupRegion(t.Context(), tc.metadata.get)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("lookupRegion: got = %q, %v, want = %q, error %t", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// fakeRegionLookup scripts the results of successive lookups and counts them.
type fakeRegionLookup struct {
	results []error // nil means the lookup finds "us-central1"
	calls   int
}

func (f *fakeRegionLookup) lookup(context.Context) (string, error) {
	err := f.results[min(f.calls, len(f.results)-1)]
	f.calls++
	if err != nil {
		return "", err
	}
	return "us-central1", nil
}

func TestRegionCache(t *testing.T) {
	t.Parallel()
	errUnreachable := errors.New("metadata server unreachable")
	// step is one get, made after advancing the clock by wait.
	type step struct {
		wait      time.Duration
		want      string
		wantCalls int
	}
	for _, tc := range []struct {
		name    string
		onGCP   bool
		results []error
		steps   []step
	}{{
		name:  "off GCP does no lookup",
		steps: []step{{want: "", wantCalls: 0}, {wait: time.Hour, want: "", wantCalls: 0}},
	}, {
		name:    "found once and cached",
		onGCP:   true,
		results: []error{nil},
		steps:   []step{{want: "us-central1", wantCalls: 1}, {wait: time.Hour, want: "us-central1", wantCalls: 1}},
	}, {
		name:    "a failure is retried after the interval",
		onGCP:   true,
		results: []error{errUnreachable, nil},
		steps: []step{
			{want: "", wantCalls: 1},
			{wait: regionRetryInterval / 2, want: "", wantCalls: 1},
			{wait: regionRetryInterval / 2, want: "us-central1", wantCalls: 2},
			{wait: time.Hour, want: "us-central1", wantCalls: 2},
		},
	}, {
		name:    "gives up after the last attempt",
		onGCP:   true,
		results: []error{errUnreachable},
		steps: []step{
			{want: "", wantCalls: 1},
			{wait: regionRetryInterval, want: "", wantCalls: 2},
			{wait: regionRetryInterval, want: "", wantCalls: regionLookupAttempts},
			{wait: time.Hour, want: "", wantCalls: regionLookupAttempts},
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			fake := &fakeRegionLookup{results: tc.results}
			cache := &regionCache{
				onGCP:  func() bool { return tc.onGCP },
				lookup: fake.lookup,
				now:    func() time.Time { return now },
			}
			for i, s := range tc.steps {
				now = now.Add(s.wait)
				if got := cache.get(t.Context()); got != s.want || fake.calls != s.wantCalls {
					t.Errorf("get %d: got = %q after %d lookups, want = %q after %d", i, got, fake.calls, s.want, s.wantCalls)
				}
			}
		})
	}
}

func TestLookupFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "not defined", err: metadata.NotDefinedError("instance/region"), want: "not defined"},
		{name: "deadline", err: fmt.Errorf("get: %w", context.DeadlineExceeded), want: "timeout"},
		{name: "other", err: errors.New("connection refused"), want: "request failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lookupFailure(tc.err); got != tc.want {
				t.Errorf("lookupFailure: got = %q, want = %q", got, tc.want)
			}
		})
	}
}

// recordingTracer keeps the completed trace.
type recordingTracer struct{ trace *agenttrace.Trace[string] }

func (r *recordingTracer) NewTrace(ctx context.Context, prompt string, opts ...agenttrace.StartTraceOption) *agenttrace.Trace[string] {
	return agenttrace.NewDefaultTracer[string](ctx).NewTrace(ctx, prompt, opts...)
}

func (r *recordingTracer) RecordTrace(trace *agenttrace.Trace[string]) { r.trace = trace }

func TestRecordCallLatency(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	valid := agenttrace.TurnLatency{RequestStartTime: start, ConnReused: new(true), FirstByteMS: new(int64(420))}
	for _, tc := range []struct {
		name        string
		latency     agenttrace.TurnLatency
		region      string
		stamped     string
		wantLatency *agenttrace.TurnLatency
		wantRegion  string
	}{
		{name: "latency and region", latency: valid, region: "us-central1", wantLatency: &valid, wantRegion: "us-central1"},
		{name: "off GCP keeps a stamped region", latency: valid, stamped: "us-east5", wantLatency: &valid, wantRegion: "us-east5"},
		{name: "no request observed", region: "us-central1", wantRegion: "us-central1"},
		{
			name:       "invalid latency is dropped",
			latency:    agenttrace.TurnLatency{RequestStartTime: start, ConnectMS: new(int64(-1))},
			region:     "us-central1",
			wantRegion: "us-central1",
		},
		{name: "invalid region is dropped", latency: valid, region: "US Central", wantLatency: &valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tracer := &recordingTracer{}
			ctx := agenttrace.WithTracer[string](t.Context(), tracer)
			trace, done := agenttrace.StartTrace[string](ctx, "synthetic fixture")
			if tc.stamped != "" {
				if err := trace.RecordClientRegion(tc.stamped); err != nil {
					t.Fatalf("RecordClientRegion: %v", err)
				}
			}
			turn := trace.BeginTurn(0, "fixture-provider", "fixture-model")
			recordCallLatency(ctx, trace, turn, tc.latency, tc.region)
			turn.End()
			done("done", nil)

			if diff := cmp.Diff(tc.wantLatency, tracer.trace.Turns[0].Latency); diff != "" {
				t.Errorf("latency (-want +got):\n%s", diff)
			}
			if got := tracer.trace.ClientRegion; got != tc.wantRegion {
				t.Errorf("ClientRegion: got = %q, want = %q", got, tc.wantRegion)
			}
		})
	}
}

func TestLatencySwitch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "unset", want: true},
		{name: "true", value: "true", want: true},
		{name: "one", value: "1", want: true},
		{name: "false", value: "false", want: false},
		{name: "zero", value: "0", want: false},
		{name: "upper case", value: "FALSE", want: false},
		{name: "padded", value: " false\n", want: false},
		{name: "not a boolean", value: "off", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var keys []string
			sw := &latencySwitch{getenv: func(key string) string {
				keys = append(keys, key)
				return tc.value
			}}
			for range 2 {
				if got := sw.enabled(t.Context()); got != tc.want {
					t.Errorf("enabled: got = %t, want = %t", got, tc.want)
				}
			}
			// The name is what operators set, so it is spelled out rather than
			// read from callLatencyEnv.
			if diff := cmp.Diff([]string{"DRIFTLESSAF_TRACE_CALL_LATENCY"}, keys); diff != "" {
				t.Errorf("getenv calls (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRecordCallLatencyOptOut(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	valid := agenttrace.TurnLatency{RequestStartTime: start, ConnReused: new(true), FirstByteMS: new(int64(420))}
	for _, tc := range []struct {
		name        string
		value       string
		wantLatency *agenttrace.TurnLatency
		wantRegion  string
		wantLookups int
	}{
		{name: "on by default", wantLatency: &valid, wantRegion: "us-central1", wantLookups: 1},
		{name: "turned off", value: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRegionLookup{results: []error{nil}}
			regions := &regionCache{
				onGCP:  func() bool { return true },
				lookup: fake.lookup,
				now:    func() time.Time { return start },
			}
			sw := &latencySwitch{getenv: func(string) string { return tc.value }}
			tracer := &recordingTracer{}
			ctx := agenttrace.WithTracer[string](t.Context(), tracer)
			trace, done := agenttrace.StartTrace[string](ctx, "synthetic fixture")
			turn := trace.BeginTurn(0, "fixture-provider", "fixture-model")
			recordIfEnabled(ctx, sw, regions, trace, turn, valid)
			turn.End()
			done("done", nil)

			if diff := cmp.Diff(tc.wantLatency, tracer.trace.Turns[0].Latency); diff != "" {
				t.Errorf("latency (-want +got):\n%s", diff)
			}
			if got := tracer.trace.ClientRegion; got != tc.wantRegion {
				t.Errorf("ClientRegion: got = %q, want = %q", got, tc.wantRegion)
			}
			if fake.calls != tc.wantLookups {
				t.Errorf("region lookups: got = %d, want = %d", fake.calls, tc.wantLookups)
			}
		})
	}
}
