/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const testModel = "claude-mythos-5-1"

// call is one scripted model turn: a tool call, or with block a turn that
// never answers until its request is cancelled. do runs before the turn
// answers.
type call struct {
	name  string
	args  string
	do    func()
	block bool
}

// model is a scripted Anthropic Messages server. A request takes its turn
// from the lane whose key its body carries, so concurrent delegates each
// follow their own script.
type model struct {
	t      *testing.T
	mu     sync.Mutex
	lanes  map[string][]call
	bodies map[string][]string
	served int
}

func (m *model) lane(key string, calls ...call) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lanes[key] = append(m.lanes[key], calls...)
}

func (m *model) requests(key string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.bodies[key])
}

func (m *model) next(body string) (c call, n int, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.served++
	for key, script := range m.lanes {
		if !strings.Contains(body, key) {
			continue
		}
		m.bodies[key] = append(m.bodies[key], body)
		if len(script) == 0 {
			return call{}, m.served, false
		}
		m.lanes[key] = script[1:]
		return script[0], m.served, true
	}
	return call{}, m.served, false
}

func (m *model) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c, n, ok := m.next(string(body))
	if !ok {
		m.t.Errorf("model request %d has no scripted turn", n)
		http.Error(w, "script exhausted", http.StatusInternalServerError)
		return
	}
	if c.do != nil {
		c.do()
	}
	if c.block {
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(e map[string]any) {
		data, _ := json.Marshal(e)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e["type"], data)
	}
	event(map[string]any{"type": "message_start", "message": map[string]any{
		"id": fmt.Sprintf("msg-%d", n), "type": "message", "role": "assistant", "model": testModel,
		"content": []any{}, "usage": map[string]any{"input_tokens": 1000, "output_tokens": 1},
	}})
	event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{
		"type": "tool_use", "id": fmt.Sprintf("tool-%d", n), "name": c.name, "input": map[string]any{},
	}})
	event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": c.args}})
	event(map[string]any{"type": "content_block_stop", "index": 0})
	event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 100}})
	event(map[string]any{"type": "message_stop"})
}

// brief binds a delegate's prompt as its first user message.
type brief struct{ prompt string }

func (b *brief) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p.BindJSON("prompt", b.prompt)
}

// harness is a Pool whose delegates run a real metaagent against the
// scripted model, with one tool, look, and records what reaches the
// caller's hooks. A test's own Options.Tools or Options.Execute replaces
// the harness's.
type harness struct {
	t      *testing.T
	model  *model
	pool   *Pool
	cancel context.CancelFunc
	tools  map[string]toolcall.Tool[string]
	trace  *agenttrace.Trace[string]

	mu      sync.Mutex
	stopped []Result
	traced  []agenttrace.RecordedTurn
	agents  []string
}

// lookTool is the delegates' one tool: it echoes what.
func lookTool() toolcall.Tool[*Report] {
	return toolcall.Tool[*Report]{
		Def: toolcall.Definition{Name: "look", Description: "Look at something.",
			Parameters: []toolcall.Parameter{{Name: "what", Type: "string"}}},
		Handler: func(_ context.Context, call toolcall.ToolCall, _ *agenttrace.Trace[*Report], _ **Report) map[string]any {
			what, _ := toolcall.OptionalParam(call, "what", "")
			return map[string]any{"saw": what}
		},
	}
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	m := &model{t: t, lanes: map[string][]call{}, bodies: map[string][]string{}}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	const prov modelrouter.Provider = "scripted"
	rt, err := metaagent.NewRuntime([]modelrouter.Route{{
		Selection:       modelrouter.Selection{Provider: prov, LogicalModel: testModel},
		Protocol:        modelrouter.ProtocolAnthropicMessages,
		ProviderModelID: testModel,
		Attribution:     modelrouter.Attribution{ProviderName: "gcp.vertex_ai", LegacySystem: agenttrace.SystemGoogleVertex},
		Capabilities: modelrouter.Capabilities{
			Efforts: []effort.Level{effort.High}, PromptCaching: true, ToolCalling: true, TerminalSubmission: true,
			MaximumOutputTokens: true,
		},
	}}, metaagent.NewAdapterBackend("", prov, func(string) (metaagent.AdapterRegistrations, error) {
		return metaagent.AdapterRegistrations{AnthropicMessages: []metaagent.AnthropicMessagesRegistration{{
			Provider: prov,
			Adapter: func(_ context.Context, plan modelrouter.Plan) (metaagent.AnthropicMessagesBinding, error) {
				client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("scripted"), option.WithMaxRetries(0))
				return metaagent.NewAnthropicMessagesBinding(plan, client.Messages, nil)
			},
		}}}, nil
	}))
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	target := rt.Target(metaagent.TargetConfig{Provider: prov, Model: testModel})

	h := &harness{t: t, model: m}
	if opts.Tools == nil {
		opts.Tools = func(context.Context, string) (map[string]toolcall.Tool[*Report], error) {
			return map[string]toolcall.Tool[*Report]{"look": lookTool()}, nil
		}
	}
	if opts.Execute == nil {
		opts.Execute = func(ctx context.Context, prompt string, tools toolcall.ToolProvider[*Report, toolcall.EmptyTools]) (*Report, error) {
			agent, err := metaagent.NewWithTarget[*brief](ctx, target, metaagent.Config[*Report, toolcall.EmptyTools]{
				SystemInstructions:  promptbuilder.MustNewPrompt("You are a delegate."),
				UserPrompt:          promptbuilder.MustNewPrompt("{{prompt}}"),
				Tools:               tools,
				MaxTurns:            20,
				Effort:              effort.High,
				MaxTokens:           4096,
				OmitSubmitReasoning: true,
			})
			if err != nil {
				return nil, err
			}
			return agent.Execute(ctx, &brief{prompt: prompt}, toolcall.EmptyTools{})
		}
	}
	opts.OnStop = func(_ context.Context, r Result) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.stopped = append(h.stopped, r)
	}
	opts.OnTrace = func(tr *agenttrace.Trace[*Report]) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.traced = append(h.traced, tr.Turns...)
		h.agents = append(h.agents, tr.AgentName)
	}
	ctx, cancel := context.WithCancel(t.Context())
	h.cancel = cancel
	h.pool, err = New(ctx, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { cancel(); h.pool.Wait() })
	h.tools = Tools[string](h.pool)
	h.trace = agenttrace.ByCode[string]().NewTrace(t.Context(), "parent")
	return h
}

// call runs the parent's tool name with args.
func (h *harness) call(name string, args map[string]any) map[string]any {
	return h.tools[name].Handler(h.t.Context(), toolcall.ToolCall{ID: "parent-" + name, Name: name, Args: args}, h.trace, nil)
}

// start starts a delegate and returns its id.
func (h *harness) start(args map[string]any) string {
	h.t.Helper()
	res := h.call(ToolDelegate, args)
	id, _ := res["id"].(string)
	if id == "" {
		h.t.Fatalf("delegate: got = %v, want an id", res)
	}
	return id
}

// result waits for the delegate with id to stop and returns what OnStop got.
func (h *harness) result(id string) Result {
	h.t.Helper()
	var r Result
	waitFor(h.t, "delegate "+id+" to stop", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, s := range h.stopped {
			if s.ID == id {
				r = s
				return true
			}
		}
		return false
	})
	return r
}

func (h *harness) tracedInput() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var n int64
	for _, t := range h.traced {
		n += t.InputTokens
	}
	return n
}

// waitFor polls cond until it holds, failing the test after 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// hold returns a turn hook that blocks until release is closed.
func hold(t *testing.T, release <-chan struct{}) func() {
	return func() {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			t.Error("never released")
		}
	}
}

// TestReportReachesOnStop follows one delegate: delegate returns its id at
// once; the delegate runs its prompt with the caller's tools; OnStop gets
// its report and a transcript with its steps; its turns reach OnTrace; and
// the parent's trace records the delegate call.
func TestReportReachesOnStop(t *testing.T) {
	h := newHarness(t, Options{})
	release := make(chan struct{})
	h.model.lane("DELEGATE-A",
		call{name: "look", args: `{"reasoning":"LOOKING-A","what":"WHAT-A"}`, do: hold(t, release)},
		call{name: "report", args: `{"result":{"text":"REPORT-A"}}`})
	id := h.start(map[string]any{"title": "TITLE-A", "prompt": "DELEGATE-A: map the parser"})
	if n := h.pool.Running(); n != 1 {
		t.Errorf("Running before the report: got = %d, want = 1", n)
	}
	close(release)
	r := h.result(id)
	if r.State != Done || r.Title != "TITLE-A" || r.Report != "REPORT-A" {
		t.Errorf("result: got = %+v, want done, TITLE-A, REPORT-A", r)
	}
	for _, want := range []string{"LOOKING-A", `"what":"WHAT-A"`, `"saw":"WHAT-A"`, "REPORT-A", "DELEGATE-A: map the parser"} {
		if !strings.Contains(r.Transcript, want) {
			t.Errorf("transcript lacks %q: %s", want, r.Transcript)
		}
	}
	reqs := h.model.requests("DELEGATE-A")
	if len(reqs) != 2 {
		t.Fatalf("delegate requests: got = %d, want = 2", len(reqs))
	}
	for _, name := range []string{"look", "report"} {
		if !strings.Contains(reqs[0], `"name":"`+name+`"`) {
			t.Errorf("the delegate has no %s tool", name)
		}
	}
	h.pool.Wait()
	if got := h.tracedInput(); got != 2000 {
		t.Errorf("input tokens traced: got = %d, want = 2000", got)
	}
	if n := h.pool.Running(); n != 0 {
		t.Errorf("Running after the report: got = %d, want = 0", n)
	}
	if n := len(h.trace.ToolCalls); n != 1 {
		t.Errorf("parent trace tool calls: got = %d, want = 1", n)
	}
}

// TestRunConcurrently pins that delegates run side by side: each one's
// first turn waits for the other's, which it could not if they ran in turn.
func TestRunConcurrently(t *testing.T) {
	h := newHarness(t, Options{})
	var arrived sync.WaitGroup
	arrived.Add(2)
	both := make(chan struct{})
	go func() { arrived.Wait(); close(both) }()
	barrier := func() {
		arrived.Done()
		select {
		case <-both:
		case <-time.After(10 * time.Second):
			t.Error("the delegates did not run at the same time")
		}
	}
	var ids []string
	for _, key := range []string{"DELEGATE-A", "DELEGATE-B"} {
		h.model.lane(key, call{name: "look", args: `{}`, do: barrier},
			call{name: "report", args: `{"result":{"text":"REPORT-OF-` + key + `"}}`})
		ids = append(ids, h.start(map[string]any{"title": "T", "prompt": key}))
	}
	for i, id := range ids {
		if r := h.result(id); r.State != Done || !strings.HasSuffix(r.Report, []string{"DELEGATE-A", "DELEGATE-B"}[i]) {
			t.Errorf("result %d: got = %+v", i, r)
		}
	}
}

// TestTimesOut pins the time box: it is minutes long, and a delegate whose
// model turn outlasts it stops as timed_out, with a report that carries the
// reasons it gave so far. The time box ends when the second turn arrives
// rather than on a real timer, so a slow runner cannot end it early.
func TestTimesOut(t *testing.T) {
	h := newHarness(t, Options{})
	expire := make(chan struct{})
	var boxed time.Duration
	h.pool.timeBox = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		boxed = d
		ctx, cancel := context.WithCancelCause(ctx)
		go func() {
			select {
			case <-expire:
				cancel(errTimedOut)
			case <-ctx.Done():
			}
		}()
		return ctx, func() { cancel(nil) }
	}
	h.model.lane("DELEGATE-T", call{name: "look", args: `{"reasoning":"PARTIAL-THOUGHT"}`},
		call{block: true, do: func() { close(expire) }})
	r := h.result(h.start(map[string]any{"title": "T", "prompt": "DELEGATE-T", "minutes": float64(2)}))
	if boxed != 2*time.Minute {
		t.Errorf("time box: got = %v, want = 2m", boxed)
	}
	if r.State != TimedOut || !strings.Contains(r.Report, "PARTIAL-THOUGHT") || !strings.Contains(r.Report, "time box") {
		t.Errorf("result: got = %+v, want timed_out with the partial reasons", r)
	}
	h.pool.Wait()
	// The executor records the trace on a timeout too, so the first turn's
	// tokens reach OnTrace; the turn cut off reported none.
	if got := h.tracedInput(); got != 1000 {
		t.Errorf("input tokens traced: got = %d, want = 1000", got)
	}
}

// TestCancelTool pins delegate_cancel: it stops a delegate mid-turn and
// returns its final state, and OnStop still runs, with the state cancelled.
func TestCancelTool(t *testing.T) {
	h := newHarness(t, Options{})
	h.model.lane("DELEGATE-C", call{block: true})
	id := h.start(map[string]any{"title": "T", "prompt": "DELEGATE-C"})
	waitFor(t, "the delegate's first turn", func() bool { return len(h.model.requests("DELEGATE-C")) == 1 })
	if got := h.call(ToolCancel, map[string]any{"id": id}); got["state"] != Cancelled {
		t.Errorf("delegate_cancel: got = %v, want cancelled", got)
	}
	if r := h.result(id); r.State != Cancelled {
		t.Errorf("result: got = %+v, want cancelled", r)
	}
	if got := h.call(ToolCancel, map[string]any{"id": id}); got["state"] != Cancelled {
		t.Errorf("delegate_cancel of a stopped delegate: got = %v, want cancelled", got)
	}
}

// TestStopsWithPool pins that cancelling the Pool's context stops its
// delegates, and Wait returns once their OnStop has.
func TestStopsWithPool(t *testing.T) {
	h := newHarness(t, Options{})
	h.model.lane("DELEGATE-P", call{block: true})
	id := h.start(map[string]any{"title": "T", "prompt": "DELEGATE-P"})
	waitFor(t, "the delegate's first turn", func() bool { return len(h.model.requests("DELEGATE-P")) == 1 })
	h.cancel()
	h.pool.Wait()
	if r := h.result(id); r.State != Cancelled {
		t.Errorf("result: got = %+v, want cancelled", r)
	}
}

// TestLiveTurns pins that a running delegate's paid turns are visible
// through LiveTurns before its trace is recorded, and move to OnTrace once
// it is, never counted twice.
func TestLiveTurns(t *testing.T) {
	h := newHarness(t, Options{})
	release := make(chan struct{})
	h.model.lane("DELEGATE-S", call{name: "look", args: `{}`}, call{name: "look", args: `{}`},
		call{name: "report", args: `{"result":{"text":"R"}}`, do: hold(t, release)})
	id := h.start(map[string]any{"title": "T", "prompt": "DELEGATE-S"})
	waitFor(t, "the delegate's third turn", func() bool { return len(h.model.requests("DELEGATE-S")) == 3 })
	var live int64
	for _, turn := range h.pool.LiveTurns() {
		live += turn.InputTokens
	}
	// A tool call's trace holds the turns before the one that asked for it:
	// the second tool call sees the first turn.
	if live != 1000 || h.tracedInput() != 0 {
		t.Errorf("while running: live = %d, traced = %d, want 1000 and 0", live, h.tracedInput())
	}
	close(release)
	h.result(id)
	h.pool.Wait()
	if n := len(h.pool.LiveTurns()); n != 0 || h.tracedInput() != 3000 {
		t.Errorf("after: live turns = %d, traced = %d, want 0 and 3000", n, h.tracedInput())
	}
}

// TestPeek pins delegate_peek: on a running delegate it shows each step's
// reason, args and result, cut to StepResultLimit; once stopped, its report.
func TestPeek(t *testing.T) {
	h := newHarness(t, Options{StepResultLimit: 20})
	release := make(chan struct{})
	long := strings.Repeat("x", 100)
	h.model.lane("DELEGATE-K", call{name: "look", args: `{"reasoning":"PEEK-ME","what":"` + long + `"}`},
		call{name: "report", args: `{"result":{"text":"REPORT-K"}}`, do: hold(t, release)})
	id := h.start(map[string]any{"title": "T", "prompt": "DELEGATE-K"})
	waitFor(t, "the delegate's second turn", func() bool { return len(h.model.requests("DELEGATE-K")) == 2 })
	got := h.call(ToolPeek, map[string]any{"id": id})
	entries, _ := got["entries"].([]map[string]any)
	if got["state"] != Running || len(entries) != 1 || entries[0]["reason"] != "PEEK-ME" ||
		!strings.Contains(entries[0]["args"].(string), long) || !strings.HasSuffix(entries[0]["result"].(string), " [cut]") {
		t.Errorf("peek while running: got = %v", got)
	}
	if _, ok := got["report"]; ok {
		t.Errorf("peek while running shows a report: %v", got)
	}
	close(release)
	h.result(id)
	h.pool.Wait()
	if got := h.call(ToolPeek, map[string]any{"id": id}); got["state"] != Done || got["report"] != "REPORT-K" {
		t.Errorf("peek once done: got = %v", got)
	}
	list := h.call(ToolList, nil)["delegates"].([]map[string]any)
	if len(list) != 1 || list[0]["id"] != id || list[0]["state"] != Done || list[0]["turns"] != 2 || list[0]["tool_calls"] != 1 {
		t.Errorf("delegates: got = %v", list)
	}
}

// TestReportCut pins that the report OnStop and peek give is cut to
// ReportLimit, and the transcript keeps all of it.
func TestReportCut(t *testing.T) {
	h := newHarness(t, Options{ReportLimit: 10})
	text := strings.Repeat("y", 50)
	h.model.lane("DELEGATE-L", call{name: "report", args: `{"result":{"text":"` + text + `"}}`})
	r := h.result(h.start(map[string]any{"title": "T", "prompt": "DELEGATE-L"}))
	if !strings.HasPrefix(r.Report, strings.Repeat("y", 10)+"\n\n[cut at 10 of 50 bytes") || !strings.Contains(r.Transcript, text) {
		t.Errorf("result: got = %+v", r)
	}
}

// TestRefusals pins the calls refused without starting anything.
func TestRefusals(t *testing.T) {
	h := newHarness(t, Options{MaxRunning: 1})
	h.model.lane("DELEGATE-R", call{block: true})
	h.start(map[string]any{"title": "T", "prompt": "DELEGATE-R"})
	for _, tc := range []struct {
		name, tool string
		args       map[string]any
		want       string
	}{
		{"over the limit", ToolDelegate, map[string]any{"title": "T", "prompt": "P"}, "1 delegates are running"},
		{"minutes too high", ToolDelegate, map[string]any{"title": "T", "prompt": "P", "minutes": float64(31)}, "minutes must be 1 to 30"},
		{"minutes too low", ToolDelegate, map[string]any{"title": "T", "prompt": "P", "minutes": float64(0)}, "minutes must be 1 to 30"},
		{"no prompt", ToolDelegate, map[string]any{"title": "T", "prompt": " "}, "delegate needs prompt"},
		{"peek unknown", ToolPeek, map[string]any{"id": "d-00000000"}, `no delegate "d-00000000"`},
		{"cancel unknown", ToolCancel, map[string]any{"id": "d-00000000"}, `no delegate "d-00000000"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := h.call(tc.tool, tc.args)["error"].(string)
			if !strings.Contains(got, tc.want) {
				t.Errorf("error: got = %q, want it to contain %q", got, tc.want)
			}
		})
	}
	if n := h.pool.Running(); n != 1 {
		t.Errorf("Running: got = %d, want = 1", n)
	}
}

// TestSharedToolsMap pins that delegates whose Options.Tools returns one
// map each record only their own calls, and leave the map as it was.
func TestSharedToolsMap(t *testing.T) {
	shared := map[string]toolcall.Tool[*Report]{"look": lookTool()}
	h := newHarness(t, Options{Tools: func(context.Context, string) (map[string]toolcall.Tool[*Report], error) {
		return shared, nil
	}})
	keys := []string{"DELEGATE-X", "DELEGATE-Y"}
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		h.model.lane(key, call{name: "look", args: `{"reasoning":"REASON-OF-` + key + `"}`},
			call{name: "report", args: `{"result":{"text":"R"}}`})
		ids = append(ids, h.start(map[string]any{"title": "T", "prompt": key}))
	}
	for i, id := range ids {
		r := h.result(id)
		other := keys[1-i]
		if r.State != Done || !strings.Contains(r.Transcript, "REASON-OF-"+keys[i]) || strings.Contains(r.Transcript, "REASON-OF-"+other) {
			t.Errorf("result %d: got = %+v, want only its own step", i, r)
		}
	}
	h.pool.Wait()
	// A delegate that wrapped the shared map's handler in place would record
	// this call as one of its own.
	shared["look"].Handler(t.Context(), toolcall.ToolCall{ID: "after", Name: "look"}, agenttrace.ByCode[*Report]().NewTrace(t.Context(), "after"), nil)
	for _, row := range h.call(ToolList, nil)["delegates"].([]map[string]any) {
		if row["tool_calls"] != 1 {
			t.Errorf("delegate %v tool calls: got = %v, want = 1", row["id"], row["tool_calls"])
		}
	}
}

// TestFails pins the failed state: an Execute that ends with an error, or
// with neither a report nor an error, while its context is live. Its report
// carries why and the reasons it gave, and its live turns drop though no
// trace was recorded.
func TestFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp *Report
		err  error
		want string
	}{
		{"error", nil, errors.New("EXECUTE-BROKE"), "EXECUTE-BROKE"},
		{"no report", nil, nil, "no report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Options{Execute: func(ctx context.Context, prompt string, tools toolcall.ToolProvider[*Report, toolcall.EmptyTools]) (*Report, error) {
				ts, err := tools.Tools(ctx, toolcall.EmptyTools{})
				if err != nil {
					return nil, err
				}
				tr := agenttrace.TracerFromContext[*Report](ctx).NewTrace(ctx, prompt)
				tr.Turns = append(tr.Turns, agenttrace.RecordedTurn{InputTokens: 1000})
				ts["look"].Handler(ctx, toolcall.ToolCall{ID: "1", Name: "look", Args: map[string]any{"reasoning": "FAILING-STEP"}}, tr, nil)
				return tc.resp, tc.err
			}})
			r := h.result(h.start(map[string]any{"title": "T", "prompt": "P"}))
			if r.State != Failed || !strings.Contains(r.Report, tc.want) || !strings.Contains(r.Report, "FAILING-STEP") {
				t.Errorf("result: got = %+v, want failed with %q and the step's reason", r, tc.want)
			}
			h.pool.Wait()
			if n := len(h.pool.LiveTurns()); n != 0 {
				t.Errorf("live turns after failing: got = %d, want = 0", n)
			}
		})
	}
}

// TestCancelReturnsWithItsContext pins that delegate_cancel gives up
// waiting when its own context ends, so a delegate that outlasts its
// cancellation cannot hang the parent; the delegate still stops later.
func TestCancelReturnsWithItsContext(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, Options{Execute: func(context.Context, string, toolcall.ToolProvider[*Report, toolcall.EmptyTools]) (*Report, error) {
		<-release
		return nil, errors.New("stopped late")
	}})
	id := h.start(map[string]any{"title": "T", "prompt": "P"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got := h.tools[ToolCancel].Handler(ctx, toolcall.ToolCall{ID: "c", Name: ToolCancel, Args: map[string]any{"id": id}}, h.trace, nil)
	if got["state"] != Running {
		t.Errorf("delegate_cancel with its context ended: got = %v, want running", got)
	}
	close(release)
	if r := h.result(id); r.State != Cancelled {
		t.Errorf("result: got = %+v, want cancelled", r)
	}
}

// TestPriceAndAgentName pins the optional hooks: Price reaches the
// transcript and the delegates listing, and AgentName names the traces.
func TestPriceAndAgentName(t *testing.T) {
	h := newHarness(t, Options{
		Price:     func(turns []agenttrace.RecordedTurn) float64 { return 1.25 * float64(len(turns)) },
		AgentName: "AGENT-N",
	})
	h.model.lane("DELEGATE-N", call{name: "report", args: `{"result":{"text":"R"}}`})
	id := h.start(map[string]any{"title": "T", "prompt": "DELEGATE-N"})
	if r := h.result(id); !strings.Contains(r.Transcript, "$1.25") {
		t.Errorf("transcript lacks the price: %s", r.Transcript)
	}
	h.pool.Wait()
	if list := h.call(ToolList, nil)["delegates"].([]map[string]any); len(list) != 1 || list[0]["usd"] != "1.25" {
		t.Errorf("delegates: got = %v, want usd 1.25", list)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !slices.Equal(h.agents, []string{"AGENT-N"}) {
		t.Errorf("trace agent names: got = %v, want = [AGENT-N]", h.agents)
	}
}

func TestNewRefuses(t *testing.T) {
	hooks := Options{
		Tools: func(context.Context, string) (map[string]toolcall.Tool[*Report], error) { return nil, nil },
		Execute: func(context.Context, string, toolcall.ToolProvider[*Report, toolcall.EmptyTools]) (*Report, error) {
			return nil, nil
		},
		OnStop: func(context.Context, Result) {},
	}
	overMax := hooks
	overMax.DefaultMinutes, overMax.MaxMinutes = 20, 10
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"no hooks", Options{}, "are required"},
		{"default over max", overMax, "DefaultMinutes 20 exceeds MaxMinutes 10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(t.Context(), tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("New: got = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if _, err := New(t.Context(), hooks); err != nil {
		t.Errorf("New with the hooks: got = %v, want nil", err)
	}
}
