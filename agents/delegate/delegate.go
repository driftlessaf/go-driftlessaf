/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package delegate

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/chainguard-dev/clog"
)

// State is a delegate's state.
type State string

// A delegate's states. Every state but Running is final.
const (
	Running   State = "running"
	Done      State = "done"
	Failed    State = "failed"
	TimedOut  State = "timed_out"
	Cancelled State = "cancelled"
)

var (
	errTimedOut  = errors.New("the delegate's time box ran out")
	errCancelled = errors.New("the delegate was cancelled")
)

// Report is a delegate's terminal submission: the report tool.
type Report struct {
	_ struct{} `submitresult:"name=report,description=Return your answer to the agent that started you. It reads only this text.,success=Reported."`

	Text string `json:"text" jsonschema:"description=Your answer in about 1-2k tokens: a headline\\, then the findings with their evidence\\, then next steps ranked. Name where any longer detail is kept.,required"`
}

// Result is a stopped delegate, as [Options.OnStop] receives it.
type Result struct {
	ID    string
	Title string
	State State
	// Report is the delegate's report cut to Options.ReportLimit. A delegate
	// that stopped without reporting leaves why it stopped and the reasons
	// it gave for its tool calls.
	Report string
	// Transcript is its prompt, every step in full and its whole report, as
	// Markdown.
	Transcript string
}

// Options configure a [Pool]. Tools, Execute and OnStop are required.
type Options struct {
	// Tools returns the tools of the delegate with id. The Pool records
	// each call for delegate_peek and the transcript.
	Tools func(ctx context.Context, id string) (map[string]toolcall.Tool[*Report], error)

	// Execute runs one delegate's conversation on prompt to its report,
	// with tools as its tool provider. ctx carries the delegate's time box,
	// a tracer the Pool reads the delegate's turns from, and the default
	// agent name; Execute must start its trace from it, as metaagent does.
	Execute func(ctx context.Context, prompt string, tools toolcall.ToolProvider[*Report, toolcall.EmptyTools]) (*Report, error)

	// OnStop is called once per delegate when it stops, before
	// [Pool.Running] stops counting it. Its context outlives the delegate's.
	OnStop func(ctx context.Context, r Result)

	// OnTrace, when set, is told of each of a delegate's traces once it is
	// recorded, for usage accounting. The executor records a trace on every
	// exit, so a delegate that times out or is cancelled still counts.
	OnTrace func(t *agenttrace.Trace[*Report])

	// Price, when set, prices turns in USD for the delegates listing and
	// the transcript.
	Price func(turns []agenttrace.RecordedTurn) float64

	// AgentName, when set, is the default agent name of the delegates'
	// traces.
	AgentName string

	// MaxRunning bounds the delegates running at once (default 8).
	MaxRunning int
	// DefaultMinutes and MaxMinutes bound a delegate's time box (defaults
	// 15 and 30).
	DefaultMinutes int
	MaxMinutes     int
	// Minute is the unit of the time box (default time.Minute).
	Minute time.Duration
	// ReportLimit bounds the report in bytes (default 16 KiB).
	ReportLimit int
	// StepResultLimit bounds each tool result delegate_peek shows in bytes
	// (default 2 KiB).
	StepResultLimit int
}

// Pool runs the delegates of one conversation. It is safe for concurrent
// use: an executor runs a turn's tool calls at the same time.
type Pool struct {
	ctx  context.Context
	opts Options

	// mu guards all and every delegate's fields below its done channel.
	mu  sync.Mutex
	all []*delegate
	wg  sync.WaitGroup

	// timeBox bounds a delegate's context to d, ending it with
	// errTimedOut. Tests replace it to end the time box on cue.
	timeBox func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)
}

func timeBox(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, d, errTimedOut)
}

// New returns a Pool whose delegates run under ctx: cancelling it stops them.
func New(ctx context.Context, opts Options) (*Pool, error) {
	if opts.Tools == nil || opts.Execute == nil || opts.OnStop == nil {
		return nil, errors.New("delegate: Tools, Execute and OnStop are required")
	}
	opts.MaxRunning = cmp.Or(opts.MaxRunning, 8)
	opts.DefaultMinutes = cmp.Or(opts.DefaultMinutes, 15)
	opts.MaxMinutes = cmp.Or(opts.MaxMinutes, 30)
	opts.Minute = cmp.Or(opts.Minute, time.Minute)
	opts.ReportLimit = cmp.Or(opts.ReportLimit, 16<<10)
	opts.StepResultLimit = cmp.Or(opts.StepResultLimit, 2<<10)
	if opts.DefaultMinutes > opts.MaxMinutes {
		return nil, fmt.Errorf("delegate: DefaultMinutes %d exceeds MaxMinutes %d", opts.DefaultMinutes, opts.MaxMinutes)
	}
	return &Pool{ctx: ctx, opts: opts, timeBox: timeBox}, nil
}

type delegate struct {
	id, title, prompt string
	start             time.Time
	cancel            context.CancelCauseFunc
	// done is closed once the delegate has its final state.
	done chan struct{}

	state   State
	end     time.Time
	traceID string
	turns   []agenttrace.RecordedTurn
	// live is the turns of its trace in flight as of its latest tool call,
	// which LiveTurns reports until the trace is recorded into turns.
	live   []agenttrace.RecordedTurn
	steps  []step
	result string
}

// step is one of a delegate's tool calls: the reason the model gave for it,
// the call and its result.
type step struct {
	reason, tool, args, result string
}

// Running counts the delegates still running.
func (p *Pool) Running() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running()
}

// running counts the delegates still running; p.mu must be held.
func (p *Pool) running() int {
	n := 0
	for _, d := range p.all {
		if d.state == Running {
			n++
		}
	}
	return n
}

// LiveTurns is the turns of every running delegate's trace in flight, which
// OnTrace has not been told of yet.
func (p *Pool) LiveTurns() []agenttrace.RecordedTurn {
	p.mu.Lock()
	defer p.mu.Unlock()
	var turns []agenttrace.RecordedTurn
	for _, d := range p.all {
		turns = append(turns, d.live...)
	}
	return turns
}

// Wait waits for every delegate to stop and its OnStop to return.
func (p *Pool) Wait() {
	p.wg.Wait()
}

func (p *Pool) start(call toolcall.ToolCall) map[string]any {
	minutes, bad := toolcall.OptionalParam(call, "minutes", p.opts.DefaultMinutes)
	if bad != nil {
		return bad
	}
	if minutes < 1 || minutes > p.opts.MaxMinutes {
		return params.Error("minutes must be 1 to %d, got %d", p.opts.MaxMinutes, minutes)
	}
	d := &delegate{id: newID(), title: arg(call, "title"), prompt: arg(call, "prompt"), start: time.Now(),
		state: Running, done: make(chan struct{})}
	ctx := p.ctx
	if p.opts.AgentName != "" {
		ctx = agenttrace.WithDefaultAgentName(ctx, p.opts.AgentName)
	}
	ctx = clog.WithValues(ctx, "delegate", d.id)
	ctx = agenttrace.WithTracer[*Report](ctx, &tracer{inner: agenttrace.TracerFromContext[*Report](ctx), p: p, d: d})
	ctx, cancel := context.WithCancelCause(ctx)
	ctx, stopTimer := p.timeBox(ctx, time.Duration(minutes)*p.opts.Minute)
	d.cancel = cancel

	p.mu.Lock()
	defer p.mu.Unlock()
	if n := p.running(); n >= p.opts.MaxRunning {
		stopTimer()
		cancel(nil)
		return params.Error("%d delegates are running, the most allowed: wait for a report or cancel one", n)
	}
	p.all = append(p.all, d)
	p.wg.Go(func() {
		defer cancel(nil)
		defer stopTimer()
		p.run(ctx, d)
	})
	return map[string]any{"id": d.id, "note": "Running. Its report reaches you with a later tool result; carry on meanwhile."}
}

// run runs d's conversation to its end and hands the outcome to OnStop.
func (p *Pool) run(ctx context.Context, d *delegate) {
	defer close(d.done)
	resp, err := p.opts.Execute(ctx, d.prompt, provider{p: p, d: d})
	if err == nil && resp == nil {
		err = errors.New("the conversation ended with no report")
	}
	state, text := Done, ""
	switch cause := context.Cause(ctx); {
	case err == nil:
		text = resp.Text
	case errors.Is(cause, errTimedOut):
		state, text = TimedOut, p.partial(d, cause)
	case cause != nil:
		// Cancelled by delegate_cancel, or stopped with the Pool's context.
		state, text = Cancelled, p.partial(d, cause)
	default:
		state, text = Failed, p.partial(d, err)
	}
	end := time.Now()
	p.mu.Lock()
	transcript := p.transcript(d, state, end, text)
	p.mu.Unlock()
	// OnStop runs before the state changes, so a caller that finds no
	// delegate running has already received every report.
	p.opts.OnStop(context.WithoutCancel(ctx), Result{ID: d.id, Title: d.title, State: state, Report: p.cut(text), Transcript: transcript})
	p.mu.Lock()
	defer p.mu.Unlock()
	d.state, d.end, d.result = state, end, text
	// Execute may stop without recording its trace, which is what clears
	// live otherwise.
	d.live = nil
}

// partial is what a delegate that stopped without reporting leaves: why it
// stopped and the reasons it gave for its tool calls.
func (p *Pool) partial(d *delegate, why error) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "It stopped without a report: %v.", why)
	if len(d.steps) > 0 {
		b.WriteString(" The reasons it gave for its tool calls, oldest first:\n")
		for _, s := range d.steps {
			if s.reason != "" {
				fmt.Fprintf(&b, "- %s\n", s.reason)
			}
		}
	}
	return b.String()
}

// cut cuts text to ReportLimit.
func (p *Pool) cut(text string) string {
	if len(text) <= p.opts.ReportLimit {
		return text
	}
	return fmt.Sprintf("%s\n\n[cut at %d of %d bytes; the delegate's transcript has all of it]",
		strings.ToValidUTF8(text[:p.opts.ReportLimit], ""), p.opts.ReportLimit, len(text))
}

// transcript is d's brief, every step in full, and its report; p.mu must
// be held.
func (p *Pool) transcript(d *delegate, state State, end time.Time, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# delegate %s: %s\n\n%s after %s, %d turns", d.id, d.title, state, end.Sub(d.start).Round(time.Second), len(d.turns))
	if p.opts.Price != nil {
		fmt.Fprintf(&b, ", $%.2f", p.opts.Price(d.turns))
	}
	fmt.Fprintf(&b, ", trace %s\n\n## Prompt\n\n%s\n\n## Steps\n", d.traceID, d.prompt)
	for i, s := range d.steps {
		fmt.Fprintf(&b, "\n### %d. %s %s\n\n%s\n\n%s\n", i+1, s.tool, s.args, s.reason, s.result)
	}
	fmt.Fprintf(&b, "\n## Report\n\n%s\n", text)
	return b.String()
}

// find is the delegate with id, or nil; p.mu must be held.
func (p *Pool) find(id string) *delegate {
	for _, d := range p.all {
		if d.id == id {
			return d
		}
	}
	return nil
}

func (p *Pool) list() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	rows := make([]map[string]any, 0, len(p.all))
	for _, d := range p.all {
		end := d.end
		if d.state == Running {
			end = time.Now()
		}
		row := map[string]any{"id": d.id, "title": d.title, "state": d.state,
			"elapsed": end.Sub(d.start).Round(time.Second).String(), "tool_calls": len(d.steps), "trace_id": d.traceID}
		if d.state != Running {
			row["turns"] = len(d.turns)
			if p.opts.Price != nil {
				row["usd"] = fmt.Sprintf("%.2f", p.opts.Price(d.turns))
			}
		}
		rows = append(rows, row)
	}
	return map[string]any{"delegates": rows}
}

func (p *Pool) peek(call toolcall.ToolCall) map[string]any {
	last, bad := toolcall.OptionalParam(call, "last", 10)
	if bad != nil {
		return bad
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.find(arg(call, "id"))
	if d == nil {
		return params.Error("no delegate %q", arg(call, "id"))
	}
	shown := d.steps[len(d.steps)-min(max(last, 1), len(d.steps)):]
	entries := make([]map[string]any, 0, len(shown))
	for _, s := range shown {
		result := s.result
		if len(result) > p.opts.StepResultLimit {
			result = strings.ToValidUTF8(result[:p.opts.StepResultLimit], "") + " [cut]"
		}
		entries = append(entries, map[string]any{"reason": s.reason, "tool": s.tool, "args": s.args, "result": result})
	}
	out := map[string]any{"state": d.state, "steps": len(d.steps), "entries": entries}
	if d.state != Running {
		out["report"] = p.cut(d.result)
	}
	return out
}

// cancel stops a delegate and waits for its final state until ctx ends. A
// delegate can outlast its cancellation, in a tool that ignores its context
// or in OnStop, and the parent's conversation must not hang on it.
func (p *Pool) cancel(ctx context.Context, call toolcall.ToolCall) map[string]any {
	p.mu.Lock()
	d := p.find(arg(call, "id"))
	p.mu.Unlock()
	if d == nil {
		return params.Error("no delegate %q", arg(call, "id"))
	}
	d.cancel(errCancelled)
	select {
	case <-d.done:
	case <-ctx.Done():
		return map[string]any{"state": Running, "note": "Cancelled, but it has not stopped yet. Its report reaches you when it does."}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return map[string]any{"state": d.state}
}

// record keeps one of d's tool calls for delegate_peek and its transcript,
// and the turns of the trace it ran under for LiveTurns. The trace is read
// here, on the delegate's own goroutine, since the executor appends to it
// between tool calls.
func (p *Pool) record(d *delegate, call toolcall.ToolCall, res map[string]any, trace *agenttrace.Trace[*Report]) {
	args := maps.Clone(call.Args)
	reason, _ := args["reasoning"].(string)
	delete(args, "reasoning")
	a, _ := json.Marshal(args)
	r, _ := json.Marshal(res)
	live := slices.Clone(trace.Turns)
	p.mu.Lock()
	defer p.mu.Unlock()
	d.steps = append(d.steps, step{reason: reason, tool: call.Name, args: string(a), result: string(r)})
	d.live = live
}

// provider serves a delegate the caller's tools, each call recorded.
type provider struct {
	p *Pool
	d *delegate
}

func (pr provider) Tools(ctx context.Context, _ toolcall.EmptyTools) (map[string]toolcall.Tool[*Report], error) {
	tools, err := pr.p.opts.Tools(ctx, pr.d.id)
	if err != nil {
		return nil, err
	}
	// The caller may return one map to every delegate, so the wrappers go
	// in a map of their own.
	out := make(map[string]toolcall.Tool[*Report], len(tools))
	for name, t := range tools {
		inner := t.Handler
		t.Handler = func(ctx context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[*Report], resp **Report) map[string]any {
			res := inner(ctx, call, trace, resp)
			pr.p.record(pr.d, call, res, trace)
			return res
		}
		out[name] = t
	}
	return out, nil
}

var _ toolcall.ToolProvider[*Report, toolcall.EmptyTools] = (*provider)(nil)

// tracer forwards a delegate's traces to the tracer installed before it,
// and keeps the delegate's trace id and recorded turns.
type tracer struct {
	inner agenttrace.Tracer[*Report]
	p     *Pool
	d     *delegate
}

func (t *tracer) NewTrace(ctx context.Context, prompt string, opts ...agenttrace.StartTraceOption) *agenttrace.Trace[*Report] {
	tr := t.inner.NewTrace(ctx, prompt, opts...)
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	t.d.traceID = tr.ID
	return tr
}

func (t *tracer) RecordTrace(tr *agenttrace.Trace[*Report]) {
	t.inner.RecordTrace(tr)
	// The live turns drop before OnTrace counts the recorded ones, so a
	// caller summing both may briefly undercount, and never counts a turn
	// twice.
	t.p.mu.Lock()
	t.d.turns = append(t.d.turns, tr.Turns...)
	t.d.live = nil
	t.p.mu.Unlock()
	if t.p.opts.OnTrace != nil {
		t.p.opts.OnTrace(tr)
	}
}

var _ agenttrace.Tracer[*Report] = (*tracer)(nil)

// newID is d-<8 hex>.
func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "d-" + hex.EncodeToString(b)
}
