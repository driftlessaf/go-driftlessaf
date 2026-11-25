/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

const (
	belowMinimumBody = `{"error":{"code":400,"message":"The minimum token count to start explicit caching is 1024.","status":"INVALID_ARGUMENT"}}`
	forbiddenBody    = `{"error":{"code":403,"message":"Permission denied on resource.","status":"PERMISSION_DENIED"}}`
)

// fakeGenReq is what the fake Gemini server saw in one generateContent request.
type fakeGenReq struct {
	cachedContent string
	hasSystem     bool
}

// fakeGemini stands in for the Gemini API: it creates, extends and expires
// cachedContents, rejects generateContent requests that reference an expired
// cache the way Vertex AI does, and can reject creates.
type fakeGemini struct {
	mu sync.Mutex

	// createExpiry is how long a created cache lives, whatever TTL was asked.
	createExpiry time.Duration
	// createStatus and createBody, when set, reject every create.
	createStatus int
	createBody   string
	// updateStatus, when set, fails every cache update.
	updateStatus int
	// updateGate, when set, holds every update until it is closed.
	updateGate    chan struct{}
	updateStarted chan struct{}
	// generated receives a signal for each generateContent request.
	generated chan struct{}
	// onGenerate runs for each generateContent request before it is
	// answered; a non-zero status replaces the answer.
	onGenerate func(n int) (int, string)

	expired                     map[string]struct{}
	seq, creates, updates, gens int
	reqs                        []fakeGenReq
}

func newFakeGemini(t *testing.T) (*fakeGemini, *httptest.Server) {
	t.Helper()
	f := &fakeGemini{
		createExpiry:  time.Hour,
		expired:       map[string]struct{}{},
		updateStarted: make(chan struct{}, 8),
		generated:     make(chan struct{}, 64),
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeGemini) expire(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired[name] = struct{}{}
}

func (f *fakeGemini) expireAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 1; i <= f.seq; i++ {
		f.expired[fmt.Sprintf("cachedContents/c%d", i)] = struct{}{}
	}
}

func (f *fakeGemini) rejectCreates(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createStatus, f.createBody = status, body
}

func (f *fakeGemini) counts() (creates, updates, gens int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.updates, f.gens
}

func (f *fakeGemini) requests() []fakeGenReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeGenReq(nil), f.reqs...)
}

func (f *fakeGemini) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(r.URL.Path, "cachedContents") && r.Method == http.MethodPost:
		f.create(w)
	case strings.Contains(r.URL.Path, "cachedContents"):
		f.update(w, r)
	default:
		f.generate(w, body)
	}
}

func (f *fakeGemini) create(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.createStatus != 0 {
		w.WriteHeader(f.createStatus)
		_, _ = io.WriteString(w, f.createBody)
		return
	}
	f.seq++
	_, _ = fmt.Fprintf(w, `{"name":"cachedContents/c%d","expireTime":%q,"usageMetadata":{"totalTokenCount":5}}`,
		f.seq, time.Now().Add(f.createExpiry).UTC().Format(time.RFC3339Nano))
}

func (f *fakeGemini) update(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.updates++
	gate, status := f.updateGate, f.updateStatus
	f.mu.Unlock()
	select {
	case f.updateStarted <- struct{}{}:
	default:
	}
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"code":500,"message":"Internal error.","status":"INTERNAL"}}`)
		return
	}
	name := "cachedContents/" + r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	_, _ = fmt.Fprintf(w, `{"name":%q,"expireTime":%q}`, name, time.Now().Add(30*time.Minute).UTC().Format(time.RFC3339Nano))
}

func (f *fakeGemini) generate(w http.ResponseWriter, body []byte) {
	f.mu.Lock()
	f.gens++
	n, hook := f.gens, f.onGenerate
	f.mu.Unlock()
	select {
	case f.generated <- struct{}{}:
	default:
	}

	var parsed struct {
		CachedContent     string          `json:"cachedContent"`
		SystemInstruction json.RawMessage `json:"systemInstruction"`
	}
	_ = json.Unmarshal(body, &parsed)

	var status int
	var answer string
	if hook != nil {
		status, answer = hook(n)
	}

	f.mu.Lock()
	f.reqs = append(f.reqs, fakeGenReq{cachedContent: parsed.CachedContent, hasSystem: len(parsed.SystemInstruction) > 0})
	_, isExpired := f.expired[parsed.CachedContent]
	if status == 0 && parsed.CachedContent != "" && isExpired {
		status = http.StatusBadRequest
		answer = fmt.Sprintf(`{"error":{"code":400,"message":"Cache content %s is expired.","status":"INVALID_ARGUMENT"}}`, parsed.CachedContent)
	}
	f.mu.Unlock()

	if status == 0 {
		status, answer = http.StatusOK, lookupTurnJSON
		if strings.Contains(string(body), `"functionResponse"`) {
			answer = submitTurnJSON
		}
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, answer)
}

func newCacheTestExecutor(t *testing.T, srv *httptest.Server, opts ...googleexecutor.Option[errCapRequest, errCapResponse]) googleexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	sysPrompt, err := promptbuilder.NewPrompt("system")
	if err != nil {
		t.Fatalf("NewPrompt (system): %v", err)
	}
	opts = append([]googleexecutor.Option[errCapRequest, errCapResponse]{
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
		googleexecutor.WithSystemInstructions[errCapRequest, errCapResponse](sysPrompt),
		googleexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](func() (googletool.SubmitMetadata[errCapResponse], error) {
			return googletool.SubmitMetadata[errCapResponse]{
				Definition: &genai.FunctionDeclaration{Name: "submit_result"},
				Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse]) toolcall.SubmitOutcome[errCapResponse] {
					answer, _ := call.Args["answer"].(string)
					return toolcall.SubmitOutcome[errCapResponse]{
						Accepted:   true,
						Response:   errCapResponse{Answer: answer},
						ToolResult: map[string]any{"success": true},
					}
				},
			}, nil
		}),
	}, opts...)
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](newTestClient(t, srv.URL), prompt, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exec
}

func cacheTestTools() map[string]googletool.Metadata[errCapResponse] {
	return map[string]googletool.Metadata[errCapResponse]{
		"lookup": {
			Definition: &genai.FunctionDeclaration{Name: "lookup"},
			Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) *genai.FunctionResponse {
				return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{"ok": true}}
			},
		},
	}
}

func executeCacheRun(t *testing.T, exec googleexecutor.Interface[errCapRequest, errCapResponse]) (errCapResponse, *recordingTracer, error) {
	t.Helper()
	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)
	resp, err := exec.Execute(ctx, errCapRequest{}, cacheTestTools())
	return resp, tracer, err
}

func TestCacheExpiredMidRunIsRecreated(t *testing.T) {
	f, srv := newFakeGemini(t)
	f.onGenerate = func(n int) (int, string) {
		if n == 2 {
			f.expire("cachedContents/c1")
		}
		return 0, ""
	}
	exec := newCacheTestExecutor(t, srv)

	resp, tracer, err := executeCacheRun(t, exec)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("Answer: got = %q, want = %q", got, want)
	}
	if creates, _, gens := f.counts(); creates != 2 || gens != 3 {
		t.Errorf("creates, generates: got = %d, %d, want = 2, 3", creates, gens)
	}
	reqs := f.requests()
	if got, want := reqs[2].cachedContent, "cachedContents/c2"; got != want {
		t.Errorf("resent request cachedContent: got = %q, want = %q", got, want)
	}
	turns := tracer.traces[0].Turns
	if len(turns) != 2 {
		t.Fatalf("turns: got = %d, want = 2", len(turns))
	}
	if turns[1].Failed {
		t.Error("turns[1].Failed: got = true, want = false")
	}
	if got, want := turns[0].CacheCreationTokens, int64(5); got != want {
		t.Errorf("turns[0].CacheCreationTokens: got = %d, want = %d", got, want)
	}
	if got, want := turns[1].CacheCreationTokens, int64(5); got != want {
		t.Errorf("turns[1].CacheCreationTokens: got = %d, want = %d", got, want)
	}
	if !strings.Contains(fmt.Sprint(turns[1].Errors), "is expired") {
		t.Errorf("turns[1].Errors: got = %v, want the expiry recorded", turns[1].Errors)
	}
}

func TestCacheExpiredFallsBackInlineWhenReplacementFails(t *testing.T) {
	f, srv := newFakeGemini(t)
	f.onGenerate = func(n int) (int, string) {
		if n == 2 {
			f.expire("cachedContents/c1")
			f.rejectCreates(http.StatusForbidden, forbiddenBody)
		}
		return 0, ""
	}
	exec := newCacheTestExecutor(t, srv)

	resp, tracer, err := executeCacheRun(t, exec)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("Answer: got = %q, want = %q", got, want)
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("generate requests: got = %d, want = 3", len(reqs))
	}
	if reqs[2].cachedContent != "" || !reqs[2].hasSystem {
		t.Errorf("resent request: got = %+v, want inline system instruction and no cache", reqs[2])
	}
	turns := tracer.traces[0].Turns
	if turns[1].Failed {
		t.Error("turns[1].Failed: got = true, want = false")
	}
	if got, want := len(turns[1].Errors), 2; got != want {
		t.Errorf("turns[1].Errors: got = %d, want = %d", got, want)
	}
}

func TestCacheRecoveryGivesUp(t *testing.T) {
	for _, tt := range []struct {
		name      string
		expireAt  int
		wantGens  int
		wantCache []int64 // CacheCreationTokens per turn
	}{{
		name:      "opening request",
		expireAt:  1,
		wantGens:  2,
		wantCache: []int64{10},
	}, {
		name:      "later request",
		expireAt:  2,
		wantGens:  3,
		wantCache: []int64{5, 5},
	}} {
		t.Run(tt.name, func(t *testing.T) {
			f, srv := newFakeGemini(t)
			f.onGenerate = func(n int) (int, string) {
				if n >= tt.expireAt {
					f.expireAll()
				}
				return 0, ""
			}
			exec := newCacheTestExecutor(t, srv)

			_, tracer, err := executeCacheRun(t, exec)
			if err == nil {
				t.Fatal("Execute: got nil error, want the expiry")
			}
			if got := strings.Count(err.Error(), "is expired"); got < 2 {
				t.Errorf("error mentions the expiry %d times, want both errors joined: %v", got, err)
			}
			if creates, _, gens := f.counts(); creates != 2 || gens != tt.wantGens {
				t.Errorf("creates, generates: got = %d, %d, want = 2, %d", creates, gens, tt.wantGens)
			}
			turns := tracer.traces[0].Turns
			if len(turns) != len(tt.wantCache) {
				t.Fatalf("turns: got = %d, want = %d", len(turns), len(tt.wantCache))
			}
			for i, want := range tt.wantCache {
				if got := turns[i].CacheCreationTokens; got != want {
					t.Errorf("turns[%d].CacheCreationTokens: got = %d, want = %d", i, got, want)
				}
			}
			if !turns[len(turns)-1].Failed {
				t.Error("last turn Failed: got = false, want = true")
			}
		})
	}
}

func TestCacheExtendedOnceAcrossConcurrentRuns(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ttl          time.Duration
		createExpiry time.Duration
	}{
		{name: "default TTL", ttl: 30 * time.Minute, createExpiry: 10 * time.Minute},
		{name: "short TTL", ttl: time.Minute, createExpiry: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeGemini(t)
			f.createExpiry = tc.createExpiry
			f.updateGate = make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(f.updateGate) }) }
			defer release()
			exec := newCacheTestExecutor(t, srv, googleexecutor.WithCacheTTL[errCapRequest, errCapResponse](tc.ttl))

			var wg sync.WaitGroup
			t.Cleanup(wg.Wait)
			for range 2 {
				wg.Go(func() {
					if _, _, err := executeCacheRun(t, exec); err != nil {
						t.Errorf("Execute: %v", err)
					}
				})
			}

			select {
			case <-f.updateStarted:
			case <-time.After(10 * time.Second):
				t.Fatal("the cache was never extended")
			}
			// One run holds the extension open, so a request arriving now comes from
			// the other run, which must not wait on it.
			select {
			case <-f.generated:
			case <-f.updateStarted:
				t.Fatal("a second run started another cache update")
			case <-time.After(10 * time.Second):
				t.Fatal("no send proceeded while the extension was in flight")
			}
			release()
			wg.Wait()

			creates, updates, _ := f.counts()
			if creates != 1 || updates != 1 {
				t.Errorf("creates, updates: got = %d, %d, want = 1, 1", creates, updates)
			}
		})
	}
}

func TestConcurrentRunsShareOneCacheReplacement(t *testing.T) {
	f, srv := newFakeGemini(t)
	bothArrived := make(chan struct{})
	var once sync.Once
	f.onGenerate = func(n int) (int, string) {
		switch n {
		case 2:
			// Both runs' opening requests are in flight on c1: expire it
			// before either is answered, so both observe the same expiry.
			f.expire("cachedContents/c1")
			once.Do(func() { close(bothArrived) })
		case 1:
			select {
			case <-bothArrived:
			case <-time.After(10 * time.Second):
			}
		}
		return 0, ""
	}
	exec := newCacheTestExecutor(t, srv)

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if _, _, err := executeCacheRun(t, exec); err != nil {
				t.Errorf("Execute: %v", err)
			}
		})
	}
	wg.Wait()

	if creates, _, _ := f.counts(); creates != 2 {
		t.Errorf("creates: got = %d, want = 2 (the original and one shared replacement)", creates)
	}
}

func TestCancelledExtensionDoesNotBlockOtherRuns(t *testing.T) {
	f, srv := newFakeGemini(t)
	f.createExpiry = 10 * time.Minute // under half of the 30 minute TTL
	f.updateGate = make(chan struct{})
	exec := newCacheTestExecutor(t, srv)

	// The first run claims the extension, then is cancelled while it is in flight.
	ctx, cancel := context.WithCancel(agenttrace.WithTracer[errCapResponse](t.Context(), &recordingTracer{}))
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = exec.Execute(ctx, errCapRequest{}, cacheTestTools())
	}()
	select {
	case <-f.updateStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the cache was never extended")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled run did not finish")
	}

	// The cancellation says nothing about the cache, so another run extends it.
	close(f.updateGate)
	if _, _, err := executeCacheRun(t, exec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, updates, _ := f.counts(); updates != 2 {
		t.Errorf("updates: got = %d, want = 2: a cancelled extension stopped later runs extending the cache", updates)
	}
}

func TestFailedExtensionIsNotRetriedAndCacheIsReused(t *testing.T) {
	f, srv := newFakeGemini(t)
	// Inside the extension window (under half of the TTL) yet far above the
	// one minute reuse margin, so slow runners cannot tip it into a replacement.
	f.createExpiry = 4 * time.Minute
	f.updateStatus = http.StatusInternalServerError
	exec := newCacheTestExecutor(t, srv, googleexecutor.WithCacheTTL[errCapRequest, errCapResponse](10*time.Minute))

	for range 2 {
		if _, _, err := executeCacheRun(t, exec); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	creates, updates, _ := f.counts()
	if creates != 1 {
		t.Errorf("creates: got = %d, want = 1: a cache inside the extension window was replaced", creates)
	}
	if updates != 1 {
		t.Errorf("updates: got = %d, want = 1: a failed extension was retried", updates)
	}
}

func TestCacheBelowModelMinimumSendsInline(t *testing.T) {
	f, srv := newFakeGemini(t)
	f.rejectCreates(http.StatusBadRequest, belowMinimumBody)
	exec := newCacheTestExecutor(t, srv)

	for i := range 3 {
		resp, _, err := executeCacheRun(t, exec)
		if err != nil {
			t.Fatalf("Execute %d: %v", i, err)
		}
		if got, want := resp.Answer, "42"; got != want {
			t.Errorf("Execute %d Answer: got = %q, want = %q", i, got, want)
		}
	}
	if creates, _, _ := f.counts(); creates != 1 {
		t.Errorf("creates: got = %d, want = 1", creates)
	}
	for i, req := range f.requests() {
		if req.cachedContent != "" || !req.hasSystem {
			t.Errorf("request %d: got = %+v, want inline system instruction and no cache", i, req)
		}
	}
}
