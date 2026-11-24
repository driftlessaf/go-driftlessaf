/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

// Bounds on the lines search_codebase returns around each match when the model
// sets context_lines. Matches are bare pointers by default to save tokens, and
// these bounds keep a broad pattern from undoing that.
const (
	// maxSearchContextLines caps context_lines.
	maxSearchContextLines = 5

	// maxSearchContextLineBytes caps each returned line. A longer line is cut
	// and marked with searchContextCut.
	maxSearchContextLineBytes = 500

	// maxSearchContextBytes caps the line text across one response. Matches
	// past it keep their pointer and carry no context.
	maxSearchContextBytes = 32 << 10

	// searchContextFileBytes is how much of each file is read to place its
	// matches. A match past it carries no context.
	searchContextFileBytes = 1 << 20

	// maxSearchContextReadBytes caps the file bytes one search reads for
	// context.
	maxSearchContextReadBytes = 8 << 20
)

// searchContextCut marks the side of a line that was cut.
const searchContextCut = "[truncated]"

// searchContext is the context attached to one page of search matches.
type searchContext struct {
	// lines is the context_lines value in effect.
	lines int

	// matches holds one entry per search match. A nil entry carries no
	// context.
	matches []*matchContext

	// note says how many matches carry no context. Empty when all do.
	note string
}

// matchContext is the line holding a match's first byte and its neighbours.
type matchContext struct {
	// line is the 1-based number of the line holding the match's first byte.
	line   int
	text   string
	before []string
	after  []string
}

// size is the line text c adds to a response.
func (c *matchContext) size() int {
	n := len(c.text)
	for _, l := range c.before {
		n += len(l)
	}
	for _, l := range c.after {
		n += len(l)
	}
	return n
}

// addTo adds c to a match's response entry.
func (c *matchContext) addTo(entry map[string]any) {
	entry["line"] = c.line
	entry["text"] = c.text
	if len(c.before) > 0 {
		entry["before"] = c.before
	}
	if len(c.after) > 0 {
		entry["after"] = c.after
	}
}

// fileLines is the start of a file, cut to whole lines.
type fileLines struct {
	data string

	// line is the 1-based number of the line holding byte pos.
	pos, line int
}

// readSearchContext returns each match's line and up to n lines either side.
// The lines are read through readFile so that whatever a caller's ReadFile
// wrapper applies, such as sanitizing untrusted content, applies to them too.
// A match carries context only when pattern still matches at its offset in
// the bytes readFile returns.
func readSearchContext(ctx context.Context, readFile func(context.Context, string, int64, int) (callbacks.ReadResult, error), pattern string, matches []callbacks.Match, n int) *searchContext {
	sc := &searchContext{lines: n, matches: make([]*matchContext, len(matches))}
	p := newSearchPattern(pattern)
	files := make(map[string]*fileLines, len(matches))
	readBudget, outBudget := maxSearchContextReadBytes, maxSearchContextBytes
	missing := 0
	for i, m := range matches {
		if outBudget == 0 {
			missing++
			continue
		}
		f, seen := files[m.Path]
		if !seen {
			f = readFileLines(ctx, readFile, m.Path, &readBudget)
			files[m.Path] = f
		}
		c := f.context(m, p, n)
		if c == nil {
			missing++
			continue
		}
		// Stop at the first context that does not fit, so every match past the
		// bound is bare whatever its size.
		size := c.size()
		if size > outBudget {
			outBudget = 0
			missing++
			continue
		}
		outBudget -= size
		sc.matches[i] = c
	}
	if missing > 0 {
		sc.note = fmt.Sprintf("%d of %d matches carry no context: the context size bounds were reached, the file could not be read, or the bytes read did not match the search at that offset. Use read_file at their offsets, or narrow the search.", missing, len(matches))
	}
	return sc
}

// readFileLines reads the start of path, charging the bytes read to budget. It
// returns nil when the budget is spent or the read fails.
func readFileLines(ctx context.Context, readFile func(context.Context, string, int64, int) (callbacks.ReadResult, error), path string, budget *int) *fileLines {
	limit := min(searchContextFileBytes, *budget)
	if limit <= 0 || ctx.Err() != nil {
		return nil
	}
	res, err := readFile(ctx, path, 0, limit)
	if err != nil || res.Offset != 0 {
		return nil
	}
	*budget -= len(res.Content)
	data := res.Content
	// Some backends report no NextOffset, so a window as long as the limit may
	// have stopped mid-line. Keep whole lines only.
	if res.NextOffset != nil || len(data) >= limit {
		data = data[:strings.LastIndexByte(data, '\n')+1]
	}
	if data == "" {
		return nil
	}
	return &fileLines{data: data, line: 1}
}

// context returns m's line and up to n lines either side, or nil when m starts
// outside the bytes read or p does not match there.
func (f *fileLines) context(m callbacks.Match, p searchPattern, n int) *matchContext {
	if f == nil || m.Offset < 0 || m.Offset >= int64(len(f.data)) {
		return nil
	}
	off := int(m.Offset)
	// A ReadFile wrapper that drops bytes, such as one that strips carriage
	// returns or invisible runes, moves the text away from the search's
	// offsets, so the line at off can be another line.
	if !p.matchesAt(f.data[off:], m.Length) {
		return nil
	}
	start, end := strings.LastIndexByte(f.data[:off], '\n')+1, lineEnd(f.data, off)
	c := &matchContext{line: f.lineAt(off), text: clipLine(f.text(start, end), off-start)}
	for s := start; len(c.before) < n && s > 0; {
		prev := strings.LastIndexByte(f.data[:s-1], '\n') + 1
		c.before = append(c.before, clipLine(f.text(prev, s-1), 0))
		s = prev
	}
	slices.Reverse(c.before)
	for e := end; len(c.after) < n && e+1 < len(f.data); {
		next := lineEnd(f.data, e+1)
		c.after = append(c.after, clipLine(f.text(e+1, next), 0))
		e = next
	}
	return c
}

// searchPattern is a search_codebase pattern, used to check that a match is
// still at its offset in the bytes ReadFile returns.
type searchPattern struct {
	// literal is the pattern as the model wrote it. Some backends search for it
	// as plain text.
	literal string

	// re is the pattern anchored to the start of the text, or nil when the
	// pattern is not a Go regexp.
	re *regexp.Regexp
}

// newSearchPattern returns pattern as a searchPattern. Its regexp stays nil
// when pattern does not compile by itself: a pattern such as "a)|(b" would
// compile once wrapped, with another meaning.
func newSearchPattern(pattern string) searchPattern {
	p := searchPattern{literal: pattern}
	if _, err := regexp.Compile(pattern); err == nil {
		// Anchored, so that it matches only at the start of the text.
		p.re, _ = regexp.Compile(`^(?:` + pattern + `)`)
	}
	return p
}

// matchesAt reports whether text starts with a match of p that is length bytes
// long, as a regexp or as plain text.
func (p searchPattern) matchesAt(text string, length int) bool {
	if len(p.literal) == length && strings.HasPrefix(text, p.literal) {
		return true
	}
	if p.re == nil {
		return false
	}
	loc := p.re.FindStringIndex(text)
	return loc != nil && loc[1] == length
}

// lineAt returns the 1-based number of the line holding byte off. It counts
// newlines from the previous call's offset, so matches in file order cost one
// pass over the file.
func (f *fileLines) lineAt(off int) int {
	if off >= f.pos {
		f.line += strings.Count(f.data[f.pos:off], "\n")
	} else {
		f.line -= strings.Count(f.data[off:f.pos], "\n")
	}
	f.pos = off
	return f.line
}

// text returns the line in data[start:end] without a carriage return.
func (f *fileLines) text(start, end int) string {
	return strings.TrimSuffix(f.data[start:end], "\r")
}

// lineEnd returns the index of the newline ending the line that holds byte
// off, or len(data) when that line has none.
func lineEnd(data string, off int) int {
	if i := strings.IndexByte(data[off:], '\n'); i >= 0 {
		return off + i
	}
	return len(data)
}

// clipLine cuts line to maxSearchContextLineBytes, keeping the byte at col and
// some text before it in view. Cuts fall on UTF-8 character boundaries.
func clipLine(line string, col int) string {
	if len(line) <= maxSearchContextLineBytes {
		return line
	}
	const lead = maxSearchContextLineBytes / 4
	start := 0
	if col > maxSearchContextLineBytes-lead {
		start = min(col-lead, len(line)-maxSearchContextLineBytes)
	}
	end := start + maxSearchContextLineBytes
	for start < end && !utf8.RuneStart(line[start]) {
		start++
	}
	for end > start && end < len(line) && !utf8.RuneStart(line[end]) {
		end--
	}
	var b strings.Builder
	b.Grow(end - start + 2*len(searchContextCut))
	if start > 0 {
		b.WriteString(searchContextCut)
	}
	b.WriteString(line[start:end])
	if end < len(line) {
		b.WriteString(searchContextCut)
	}
	return b.String()
}
