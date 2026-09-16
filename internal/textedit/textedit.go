/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package textedit

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// notFound prefixes every error MatchIgnoringIndentation returns so callers
// that already match on it keep working.
const notFound = "old_string not found in file"

// Shift describes how the file's leading whitespace relates to old_string's on
// every non-blank line of a match: Add means the file carries Whitespace in
// addition to old_string's indentation; otherwise old_string carries it in
// addition to the file's. A zero Shift means the lines matched exactly apart
// from line endings.
type Shift struct {
	Add        bool
	Whitespace string
}

// String renders the shift for a tool result, for example "added 1 tab",
// "removed 2 spaces", or "" for the zero shift. Mixed whitespace renders with %q.
func (s Shift) String() string {
	if s.Whitespace == "" {
		return ""
	}
	verb := "removed"
	if s.Add {
		verb = "added"
	}
	n := len(s.Whitespace)
	switch {
	case strings.Trim(s.Whitespace, "\t") == "":
		return fmt.Sprintf("%s %d %s", verb, n, plural("tab", n))
	case strings.Trim(s.Whitespace, " ") == "":
		return fmt.Sprintf("%s %d %s", verb, n, plural("space", n))
	}
	return fmt.Sprintf("%s %q", verb, s.Whitespace)
}

func plural(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// Match is a region of a file that equals old_string once indentation is ignored.
type Match struct {
	Start int64 // byte offset of the region's first byte
	End   int64 // byte offset one past the region's last byte
	Line  int   // 1-based line number of Start
	Shift Shift
}

// line is one line of text split into its leading whitespace and the rest,
// with a trailing "\r" already dropped from body.
type line struct {
	ws   string
	body string
}

// fileLine is a line read from the file together with where it sits.
type fileLine struct {
	line
	start      int64  // byte offset of the line's first byte
	number     int    // 1-based line number
	text       string // the line without its "\n", trailing "\r" included
	hasNewline bool
}

func splitLine(s string) line {
	s = strings.TrimSuffix(s, "\r")
	body := strings.TrimLeft(s, " \t")
	return line{ws: s[:len(s)-len(body)], body: body}
}

// MatchIgnoringIndentation scans r line by line for the unique region whose
// lines equal old's lines after leading spaces and tabs are stripped (and a
// trailing "\r" is ignored on file lines). It succeeds only when exactly one
// region matches and the indentation difference is the same Shift on every
// non-blank line. It never reads the whole input into memory: it keeps a
// window of len(lines(old)) lines. If old ends with "\n", the region includes
// the trailing newline of its last line when the file has one.
//
// On failure the error names the reason so a caller can relay it to the agent:
//   - no region matches: reports whether the first non-blank line of old occurs
//     in the file, and if so at which line and byte offset, so the agent can
//     re-read there; otherwise says the file may have changed since it was read.
//   - more than one region matches: reports the count and the byte offsets of
//     the first two, and asks for more surrounding context.
//   - one region matches but the shift differs across lines: reports its line
//     and byte offset and asks the agent to copy the text exactly as read.
//
// Every error message begins with "old_string not found in file".
func MatchIgnoringIndentation(r io.Reader, old string) (Match, error) {
	if old == "" {
		return Match{}, errors.New(notFound + ": old_string is empty")
	}
	oldLines, oldEndsNewline := splitOld(old)
	firstBody, hasFirst := firstNonBlank(oldLines)

	var (
		window     []fileLine
		found      []candidate
		firstLine  int   // line where firstBody first occurs, 0 if not seen
		firstStart int64 // byte offset of that line
		pos        int64
		number     int
	)
	br := bufio.NewReader(r)
	for {
		raw, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return Match{}, err
		}
		if raw == "" && err != nil {
			break
		}
		number++
		text := strings.TrimSuffix(raw, "\n")
		fl := fileLine{
			line:       splitLine(text),
			start:      pos,
			number:     number,
			text:       text,
			hasNewline: len(text) < len(raw),
		}
		pos += int64(len(raw))
		if hasFirst && firstLine == 0 && fl.body == firstBody {
			firstLine, firstStart = fl.number, fl.start
		}

		window = append(window, fl)
		if len(window) > len(oldLines) {
			window = window[1:]
		}
		if len(window) == len(oldLines) {
			if c, ok := matchWindow(window, oldLines, oldEndsNewline); ok {
				found = append(found, c)
				if len(found) == 2 {
					break
				}
			}
		}
		if err != nil {
			break
		}
	}

	switch len(found) {
	case 0:
		if firstLine == 0 {
			return Match{}, errors.New(notFound + ": its first non-blank line does not occur anywhere, even ignoring indentation; the file may have changed since it was read")
		}
		return Match{}, fmt.Errorf("%s: its first non-blank line occurs at line %d (byte offset %d) but the following lines do not match; re-read the file there and copy the text exactly as read", notFound, firstLine, firstStart)
	case 1:
		c := found[0]
		if !c.consistent {
			return Match{}, fmt.Errorf("%s: the region at line %d (byte offset %d) matches it ignoring indentation, but the indentation difference is not the same on every line; copy the text exactly as read", notFound, c.Line, c.Start)
		}
		return c.Match, nil
	}
	return Match{}, fmt.Errorf("%s: ignoring indentation it matches at least %d regions (byte offsets %d and %d); include more surrounding context to make it unique", notFound, len(found), found[0].Start, found[1].Start)
}

// candidate is a region whose lines match old's once indentation is ignored.
// consistent reports whether every non-blank line carries the same Shift.
type candidate struct {
	Match
	consistent bool
}

// splitOld splits old into lines. A trailing "\n" is reported separately
// rather than producing an empty final line.
func splitOld(old string) ([]line, bool) {
	endsNewline := strings.HasSuffix(old, "\n")
	parts := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	lines := make([]line, 0, len(parts))
	for _, p := range parts {
		lines = append(lines, splitLine(p))
	}
	return lines, endsNewline
}

func firstNonBlank(lines []line) (string, bool) {
	for _, l := range lines {
		if l.body != "" {
			return l.body, true
		}
	}
	return "", false
}

// matchWindow compares the window against old's lines. ok is true when every
// line's body matches; the candidate then records the region and whether its
// shift is the same on every non-blank line.
func matchWindow(window []fileLine, old []line, oldEndsNewline bool) (candidate, bool) {
	c := candidate{consistent: true}
	seen := false
	for i, ol := range old {
		fl := window[i]
		if fl.body != ol.body {
			return candidate{}, false
		}
		if ol.body == "" || !c.consistent {
			continue
		}
		s, ok := lineShift(fl.ws, ol.ws)
		switch {
		case !ok, seen && s != c.Shift:
			c.consistent = false
			c.Shift = Shift{}
		case !seen:
			c.Shift, seen = s, true
		}
	}

	first, last := window[0], window[len(window)-1]
	c.Start, c.Line = first.start, first.number
	switch {
	case !oldEndsNewline:
		c.End = last.start + int64(len(strings.TrimSuffix(last.text, "\r")))
	case last.hasNewline:
		c.End = last.start + int64(len(last.text)) + 1
	default:
		c.End = last.start + int64(len(last.text))
	}
	return c, true
}

// lineShift relates the file's leading whitespace fw to old_string's ow on one
// line. ok is false when neither is a prefix of the other.
func lineShift(fw, ow string) (Shift, bool) {
	switch {
	case fw == ow:
		return Shift{}, true
	case strings.HasPrefix(fw, ow):
		return Shift{Add: true, Whitespace: fw[len(ow):]}, true
	case strings.HasPrefix(ow, fw):
		return Shift{Whitespace: ow[len(fw):]}, true
	}
	return Shift{}, false
}

// ShiftIndentation applies shift to every non-blank line of s: with Add it
// prepends Whitespace; otherwise it removes Whitespace from the start of a line
// that has it and leaves other lines unchanged. Blank lines are unchanged.
func ShiftIndentation(s string, shift Shift) string {
	if shift.Whitespace == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.Trim(l, " \t\r") == "" {
			continue
		}
		if shift.Add {
			lines[i] = shift.Whitespace + l
			continue
		}
		lines[i] = strings.TrimPrefix(l, shift.Whitespace)
	}
	return strings.Join(lines, "\n")
}

// LineStart returns the offset of the first byte of the line containing
// offset: one past the previous "\n", or 0 when offset is 0. It reads at most
// bound bytes backward; if no newline lies within that span, including when
// the span reaches the start of the input, it returns offset unchanged so
// inputs without newlines read exactly as they would without alignment.
func LineStart(r io.ReaderAt, offset int64, bound int) (int64, error) {
	if offset <= 0 {
		return 0, nil
	}
	buf := make([]byte, min(int64(bound), offset))
	n, err := r.ReadAt(buf, offset-int64(len(buf)))
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	idx := bytes.LastIndexByte(buf[:n], '\n')
	if idx < 0 {
		return offset, nil
	}
	return offset - int64(len(buf)) + int64(idx) + 1, nil
}

// LineEnd returns the offset one past the first "\n" at or after end, so a
// window ending there ends with a whole line, or size when end is at or past
// size. It reads at most bound bytes forward; if no newline lies within that
// span, including when the span reaches the end of the input, it returns end
// unchanged so inputs without newlines read exactly as they would without
// alignment.
func LineEnd(r io.ReaderAt, end, size int64, bound int) (int64, error) {
	if end >= size {
		return size, nil
	}
	buf := make([]byte, min(int64(bound), size-end))
	n, err := r.ReadAt(buf, end)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	idx := bytes.IndexByte(buf[:n], '\n')
	if idx < 0 {
		return end, nil
	}
	return end + int64(idx) + 1, nil
}
