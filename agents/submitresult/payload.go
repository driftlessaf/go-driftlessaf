/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/chainguard-dev/clog"
)

// reasoningDescription documents the reasoning parameter in every provider's
// submit tool schema.
const reasoningDescription = "Explain why you are confident this result is complete and accurate."

// ErrParameter marks a submit rejected before its payload could be parsed,
// for one of three causes: the arguments did not decode as JSON, a required
// parameter was absent or of the wrong JSON type, or coercion declined a
// stringified payload. Every recording wraps the cause, so a trace names
// which one fired instead of collapsing all three into one string. Consumers
// gating on the class match this sentinel with errors.Is rather than the
// message: an unparsed-arguments cause quotes model-controlled text.
var ErrParameter = errors.New("parameter error")

// payloadEchoLimit bounds the prefix of a stringified payload echoed into a
// rejection record. Such a payload runs to kilobytes — the submission that
// motivated the echo was 5,056 bytes — and the record lands in a trace, a
// span attribute and a log line. The opening bytes are what identify the
// shape, so a short prefix is enough to tell a wrapped object from a YAML
// document from a truncated write.
const payloadEchoLimit = 128

// arrivedAsString describes what the payload field actually carried, for the
// rejection record: its length and a bounded opening prefix. It reports the
// empty string when the field holds anything but a string, because the
// stringified payload is the shape worth echoing and the caller then appends
// nothing.
//
// The prefix is %q-quoted, so any ANSI or control bytes the model sent are
// escaped rather than replayed into a terminal, and it is trimmed back to a
// rune boundary so a multi-byte character split by the bound does not print
// as a replacement character.
func arrivedAsString(args map[string]any, field string) string {
	s, err := params.Extract[string](args, field)
	if err != nil {
		return ""
	}
	if len(s) <= payloadEchoLimit {
		return fmt.Sprintf("%s arrived as a %d-byte string: %q", field, len(s), s)
	}
	prefix := s[:payloadEchoLimit]
	for len(prefix) > 0 && !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return fmt.Sprintf("%s arrived as a %d-byte string beginning %q", field, len(s), prefix)
}

// buildOutcome turns decoded submit tool-call arguments into a SubmitOutcome.
// It is shared by the per-provider submit tool handlers, which differ only in
// how they acquire the argument map.
func buildOutcome[Response any](ctx context.Context, opts Options[Response], trace *agenttrace.Trace[Response], id, name string, args map[string]any) toolcall.SubmitOutcome[Response] {
	var reasoning string
	if !opts.OmitReasoning {
		var err error
		reasoning, err = params.Extract[string](args, "reasoning")
		if err != nil {
			trace.RejectedToolCall(id, name, args, fmt.Errorf("%w: %w", ErrParameter, err))
			return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("%s", err)}
		}
	}

	payloadRaw, err := params.Extract[map[string]any](args, opts.PayloadFieldName)
	if err != nil {
		coerced, ok := coerceStringPayload(args, opts.PayloadFieldName)
		if !ok {
			cause := err
			if arrived := arrivedAsString(args, opts.PayloadFieldName); arrived != "" {
				cause = fmt.Errorf("%w (%s)", err, arrived)
			}
			trace.RejectedToolCall(id, name, args, fmt.Errorf("%w: %w", ErrParameter, cause))
			return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("%s", err)}
		}
		clog.WarnContext(ctx, "Coerced stringified submit payload into an object",
			"tool", name,
			"field", opts.PayloadFieldName,
		)
		payloadRaw = coerced
	}

	// `reasoning` is free-form model prose, so on confidential input it
	// paraphrases that input. The byte count keeps the line diagnosable and
	// queryable; the reasoning itself travels on the SubmitOutcome to the trace.
	clog.InfoContext(ctx, "Submitting result",
		"tool", name,
		"reasoning_bytes", len(reasoning),
	)

	parsed, err := parsePayload[Response](payloadRaw)
	if err != nil {
		tc := trace.StartToolCall(id, name, args)
		tc.CompleteRejected(err)
		return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("%v", err)}
	}

	return toolcall.SubmitOutcome[Response]{
		Accepted:   true,
		Response:   parsed,
		Reasoning:  reasoning,
		ToolResult: successResult(opts.SuccessMessage),
	}
}

// coerceStringPayload recovers the common model mistake of JSON-encoding the
// payload object into a string instead of passing it as a nested object. It
// reports ok when the field is a string containing a JSON object, returning
// the decoded object; callers fall back to the original extraction error
// otherwise, so the model still sees the type-mismatch hint.
//
// A stringified payload often arrives double-closed — the complete object
// followed by one or more spurious `}`/`]` (the model closes both the
// payload and the string it imagined around it). Those are coerced too:
// rejecting them assumes the model resubmits correctly, but on CI run
// 30109908781 opus reproduced the same malformed shape across retries and
// then submitted a minimal payload without the artifact, which was accepted
// and zeroed the golden-eval judges.
//
// A stringified payload can also carry the closing tags of the tool-call
// markup the model was writing, such as `</analysis>` or
// `</analysis></invoke>`. Sonnet 5 left such tags on 639 of 702 stringified
// payloads across 97 loganalyzer golden-eval runs, and on CI job
// 108061537350 it fell back to placeholder payloads after the rejections,
// the same failure as above. Closing tags carry no content, so they are
// coerced too. Any other trailing content, including an opening
// tag that may hold another parameter's value, still declines coercion, so
// truncated or otherwise mangled payloads keep the corrective type-mismatch
// hint.
func coerceStringPayload(args map[string]any, field string) (map[string]any, bool) {
	s, err := params.Extract[string](args, field)
	if err != nil {
		return nil, false
	}

	dec := json.NewDecoder(strings.NewReader(s))
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, false
	}
	if !onlyClosingTokens(s[dec.InputOffset():]) {
		return nil, false
	}
	return obj, true
}

// onlyClosingTokens reports whether s holds nothing but whitespace, closing
// JSON delimiters (`}`/`]`) and closing markup tags such as `</invoke>`.
func onlyClosingTokens(s string) bool {
	for {
		s = strings.TrimLeft(s, "}] \t\r\n")
		if s == "" {
			return true
		}
		rest, ok := strings.CutPrefix(s, "</")
		if !ok {
			return false
		}
		name, after, ok := strings.Cut(rest, ">")
		if !ok || !isTagName(name) {
			return false
		}
		s = after
	}
}

// isTagName reports whether name is a non-empty run of ASCII letters, digits
// and `_`, which covers the tool-call markup tags and payload field names
// models leak, such as `invoke` and `submit_result`.
func isTagName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// parsePayload converts a raw payload object (as received from the model) into
// the strongly-typed Response. It is shared by the per-provider submit tool
// handlers so all apply exactly the same parsing rules.
func parsePayload[Response any](payloadRaw map[string]any) (Response, error) {
	var zero Response

	payloadJSON, err := json.Marshal(payloadRaw)
	if err != nil {
		return zero, fmt.Errorf("failed to marshal payload: %w", err)
	}

	dest := newResponseValue[Response]()
	if err := json.Unmarshal(payloadJSON, dest); err != nil {
		return zero, fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	if reflect.TypeFor[Response]().Kind() == reflect.Pointer {
		return dest.(Response), nil
	}
	return reflect.ValueOf(dest).Elem().Interface().(Response), nil
}

// newResponseValue allocates a fresh Response for reflection-driven JSON
// work, dereferencing pointer types so the result addresses the object the
// model submits.
func newResponseValue[Response any]() any {
	typ := reflect.TypeFor[Response]()
	if typ.Kind() == reflect.Pointer {
		return reflect.New(typ.Elem()).Interface()
	}
	return reflect.New(typ).Interface()
}

// successResult is the tool result an accepted submission carries back toward
// the model. The executor returns it only after the registered result
// validators accept the response.
func successResult(successMessage string) map[string]any {
	return map[string]any{
		"success": true,
		"message": successMessage,
	}
}
