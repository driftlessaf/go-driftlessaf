/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/anthropics/anthropic-sdk-go"
)

const ttl1h = anthropic.CacheControlEphemeralTTLTTL1h

func TestWithCacheTTL(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	tests := []struct {
		name      string
		opts      []Option[*testBindable, *testResponse]
		want      anthropic.CacheControlEphemeralTTL
		wantError string
	}{{
		name: "5 minutes is the API default",
		opts: []Option[*testBindable, *testResponse]{WithCacheTTL[*testBindable, *testResponse](5 * time.Minute)},
		want: "",
	}, {
		name: "1 hour",
		opts: []Option[*testBindable, *testResponse]{WithCacheTTL[*testBindable, *testResponse](time.Hour)},
		want: ttl1h,
	}, {
		name:      "zero",
		opts:      []Option[*testBindable, *testResponse]{WithCacheTTL[*testBindable, *testResponse](0)},
		wantError: "cache TTL must be 5m or 1h",
	}, {
		name:      "30 minutes",
		opts:      []Option[*testBindable, *testResponse]{WithCacheTTL[*testBindable, *testResponse](30 * time.Minute)},
		wantError: "cache TTL must be 5m or 1h",
	}, {
		name:      "2 hours",
		opts:      []Option[*testBindable, *testResponse]{WithCacheTTL[*testBindable, *testResponse](2 * time.Hour)},
		wantError: "cache TTL must be 5m or 1h",
	}, {
		name: "1 hour without cache control",
		opts: []Option[*testBindable, *testResponse]{
			WithCacheTTL[*testBindable, *testResponse](time.Hour),
			WithoutCacheControl[*testBindable, *testResponse](),
		},
		wantError: "incompatible with WithoutCacheControl",
	}, {
		name: "without cache control before 1 hour",
		opts: []Option[*testBindable, *testResponse]{
			WithoutCacheControl[*testBindable, *testResponse](),
			WithCacheTTL[*testBindable, *testResponse](time.Hour),
		},
		wantError: "incompatible with WithoutCacheControl",
	}, {
		name: "5 minutes without cache control",
		opts: []Option[*testBindable, *testResponse]{
			WithCacheTTL[*testBindable, *testResponse](5 * time.Minute),
			WithoutCacheControl[*testBindable, *testResponse](),
		},
		want: "",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt, tc.opts...)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("New() error: got = %v, want containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got := exec.(*executor[*testBindable, *testResponse]).cacheTTL; got != tc.want {
				t.Errorf("cacheTTL: got = %q, want = %q", got, tc.want)
			}
		})
	}
}

// TestCacheTTLOnEveryMarker checks that every marker the executor places on
// the assembled request (last tool, system, first user block) carries the
// configured 1-hour TTL, so no 5-minute entry precedes a 1-hour one.
func TestCacheTTLOnEveryMarker(t *testing.T) {
	t.Parallel()

	e := newTestExecutor(t,
		WithCacheTTL[*testBindable, *testResponse](time.Hour),
		WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)),
		WithCacheFirstUserBlock[*testBindable, *testResponse](),
	)
	params, _, err := e.assembleParams("rendered prompt", "", twoTools())
	if err != nil {
		t.Fatalf("assembleParams() error = %v", err)
	}

	if got := params.Tools[len(params.Tools)-1].OfTool.CacheControl.TTL; got != ttl1h {
		t.Errorf("last tool TTL: got = %q, want = %q", got, ttl1h)
	}
	if got := params.System[0].CacheControl.TTL; got != ttl1h {
		t.Errorf("system TTL: got = %q, want = %q", got, ttl1h)
	}
	if got := params.Messages[0].Content[0].OfText.CacheControl.TTL; got != ttl1h {
		t.Errorf("first user block TTL: got = %q, want = %q", got, ttl1h)
	}

	body := string(marshalParams(t, params))
	markers, withTTL := strings.Count(body, `"cache_control"`), strings.Count(body, `"ttl":"1h"`)
	if markers != 3 || withTTL != markers {
		t.Errorf("markers: got %d cache_control and %d with ttl 1h, want 3 of each: %s", markers, withTTL, body)
	}
}

// TestCacheTTLDefaultLeavesRequestUnchanged is the compatibility check: an
// executor without WithCacheTTL, or with the 5-minute default, sends no ttl
// field, so its request bytes match those from before the option existed.
func TestCacheTTLDefaultLeavesRequestUnchanged(t *testing.T) {
	t.Parallel()

	assemble := func(opts ...Option[*testBindable, *testResponse]) []byte {
		e := newTestExecutor(t, append([]Option[*testBindable, *testResponse]{
			WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)),
			WithCacheFirstUserBlock[*testBindable, *testResponse](),
		}, opts...)...)
		params, _, err := e.assembleParams("rendered prompt", "", twoTools())
		if err != nil {
			t.Fatalf("assembleParams() error = %v", err)
		}
		return marshalParams(t, params)
	}

	unset := assemble()
	if strings.Contains(string(unset), `"ttl"`) {
		t.Errorf("default request carries a ttl field: %s", unset)
	}
	if fiveMinutes := assemble(WithCacheTTL[*testBindable, *testResponse](5 * time.Minute)); !bytes.Equal(unset, fiveMinutes) {
		t.Errorf("WithCacheTTL(5m) changed the request:\n unset: %s\n   5m: %s", unset, fiveMinutes)
	}
}

// TestCacheTTLRaisesCallerMarkers checks that, under the 1-hour TTL, markers
// a caller placed on its own tool definitions are raised to 1 hour on the
// request (they precede the executor's markers in the prefix), while the
// caller's definitions keep their own value.
func TestCacheTTLRaisesCallerMarkers(t *testing.T) {
	t.Parallel()

	tools := callerMarkedTools(2)
	e := newTestExecutor(t, WithCacheTTL[*testBindable, *testResponse](time.Hour))
	params, _, err := e.assembleParams("rendered prompt", "", tools)
	if err != nil {
		t.Fatalf("assembleParams() error = %v", err)
	}
	for _, tool := range params.Tools {
		cc := tool.OfTool.CacheControl
		if hasBreakpoint(cc) && cc.TTL != ttl1h {
			t.Errorf("tool %q marker TTL: got = %q, want = %q", tool.OfTool.Name, cc.TTL, ttl1h)
		}
	}
	for name, meta := range tools {
		if got := meta.Definition.CacheControl.TTL; got != "" {
			t.Errorf("caller tool %q TTL mutated: got = %q, want = %q", name, got, "")
		}
	}

	// Under the default TTL the caller's markers pass through unchanged.
	callerHour := callerMarkedTools(1)
	meta := callerHour["alpha"]
	meta.Definition.CacheControl.TTL = ttl1h
	callerHour["alpha"] = meta
	params, _, err = newTestExecutor(t).assembleParams("rendered prompt", "", callerHour)
	if err != nil {
		t.Fatalf("assembleParams() error = %v", err)
	}
	if got := params.Tools[0].OfTool.CacheControl.TTL; got != ttl1h {
		t.Errorf("caller 1h marker under the default TTL: got = %q, want = %q", got, ttl1h)
	}
}

func TestTailBreakpointsCarryTTL(t *testing.T) {
	t.Parallel()

	tail := newTailBreakpoints(anthropic.MessageNewParams{}, ttl1h)
	messages := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("first"))}
	tail.advance(messages)
	if got := messages[0].Content[0].OfText.CacheControl.TTL; got != ttl1h {
		t.Errorf("tail marker TTL: got = %q, want = %q", got, ttl1h)
	}

	// Rotating past the limit clears the oldest marker entirely.
	for _, text := range []string{"second", "third"} {
		messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(text)))
		tail.advance(messages)
	}
	if hasBreakpoint(messages[0].Content[0].OfText.CacheControl) {
		t.Error("oldest tail marker survived rotation")
	}
	if got := messages[2].Content[0].OfText.CacheControl.TTL; got != ttl1h {
		t.Errorf("newest tail marker TTL: got = %q, want = %q", got, ttl1h)
	}
}

// TestConfigDigestIgnoresCacheTTL checks that the suspend/resume config digest
// does not depend on the cache TTL, so an envelope parked under one TTL
// validates under another, while a real prefix change still alters it.
func TestConfigDigestIgnoresCacheTTL(t *testing.T) {
	t.Parallel()

	digest := func(opts ...Option[*testBindable, *testResponse]) string {
		e := newTestExecutor(t, append([]Option[*testBindable, *testResponse]{
			WithSystemInstructions[*testBindable, *testResponse](systemInstructions(t)),
		}, opts...)...)
		d, err := e.configDigest(callerMarkedTools(1))
		if err != nil {
			t.Fatalf("configDigest() error = %v", err)
		}
		return d
	}

	fiveMinutes := digest()
	if oneHour := digest(WithCacheTTL[*testBindable, *testResponse](time.Hour)); oneHour != fiveMinutes {
		t.Errorf("digest depends on the cache TTL: 5m = %s, 1h = %s", fiveMinutes, oneHour)
	}
	if otherModel := digest(WithModel[*testBindable, *testResponse]("claude-opus-4-8")); otherModel == fiveMinutes {
		t.Error("digest ignored a model change")
	}
}

func TestServingAttributionCacheTTL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []Option[*testBindable, *testResponse]
		want *int64
	}{{
		name: "default",
		want: new(int64(300)),
	}, {
		name: "1 hour",
		opts: []Option[*testBindable, *testResponse]{WithCacheTTL[*testBindable, *testResponse](time.Hour)},
		want: new(int64(3600)),
	}, {
		name: "without cache control",
		opts: []Option[*testBindable, *testResponse]{WithoutCacheControl[*testBindable, *testResponse]()},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newTestExecutor(t, tc.opts...)
			got := e.servingAttribution().Serving.CacheTTLSeconds
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("CacheTTLSeconds: got = %d, want = nil", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Errorf("CacheTTLSeconds: got = %v, want = %d", got, *tc.want)
			}
			if e.attribution.Serving.CacheTTLSeconds != nil {
				t.Error("servingAttribution mutated the executor's attribution")
			}
		})
	}
}
