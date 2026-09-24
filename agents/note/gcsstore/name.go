/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstore

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"chainguard.dev/driftlessaf/agents/note"
)

// errNotANote marks an object name that does not have the note layout's shape at
// all — the wrong number of segments below the root, or a segment missing its
// role prefix. A store's root may be shared with other data (skillchain keeps a
// version's notes beside its hardened tree and manifests), so a listing skips
// these rather than failing.
//
// It is deliberately distinct from the errors a note-shaped name returns. Once a
// name has the shape, a bad run number, a coordinate that fails
// [note.Note.Validate], a malformed escape, or a non-canonical spelling are all
// fatal: those are corruption or the aliasing the encoding exists to prevent,
// and skipping them would hide exactly the bug the canonical check catches.
var errNotANote = errors.New("gcsstore: not a note object")

// Each coordinate occupies one path segment carrying a role prefix. The prefix
// keeps every segment non-empty, which is what lets the shared scope (Author "")
// encode as a bare "author_" rather than a trailing slash, and it keeps a
// coordinate from being read as a path element in its own right — a Key of ".."
// lands in the segment "key_..", never "..".
const (
	keySegment    = "key_"
	runSegment    = "run_"
	nameSegment   = "name_"
	authorSegment = "author_"
)

// segmentCount is the number of path segments below the root: key, run, name,
// author.
const segmentCount = 4

// objectName returns the object name a note's coordinates are stored under:
//
//	<root>key_<esc>/run_<N>/name_<esc>/author_<esc>
//
// Each string coordinate is escaped with [url.PathEscape], which never emits "/"
// and escapes "%" as "%25". That makes the mapping from coordinates to object
// name injective, so path equality is coordinate equality — the property [note]
// gets from Ref, held here by the encoding instead. An unescaped layout would
// alias: ("a/b", "c") and ("a", "b/c") frame identically when the coordinates
// are simply joined with "/".
func objectName(root, key string, run int, name, author string) string {
	return root +
		keySegment + url.PathEscape(key) + "/" +
		runSegment + strconv.Itoa(run) + "/" +
		nameSegment + url.PathEscape(name) + "/" +
		authorSegment + url.PathEscape(author)
}

// parseObjectName recovers the coordinates an object name encodes, so a prefix
// scan can report notes without reading their bodies.
//
// It re-encodes what it decoded and requires the result to equal the input. That
// single check is what makes the mapping a bijection rather than merely
// injective: escaping is one-to-one in the encode direction, but several inputs
// decode to the same coordinates ("run_01" and "run_1", or "%2f" and "%2F", the
// latter being what PathEscape emits). Without the check, two distinct object
// names would report the same note — the aliasing the layout exists to prevent,
// reintroduced through the decode direction.
//
// A name without the note layout's shape returns [errNotANote], which a listing
// skips; a name with the shape but bad content is fatal. See errNotANote for why
// the two are answered differently.
func parseObjectName(root, objName string) (note.Note, error) {
	rest, ok := strings.CutPrefix(objName, root)
	if !ok {
		return note.Note{}, fmt.Errorf("%w: object %q is not under the store root %q", errNotANote, objName, root)
	}

	segments := strings.Split(rest, "/")
	if len(segments) != segmentCount {
		return note.Note{}, fmt.Errorf("%w: object %q has %d path segments below the root, want %d", errNotANote, objName, len(segments), segmentCount)
	}
	// Check the complete layout before decoding any content: a foreign object
	// can have malformed coordinates before a segment that lacks a role prefix.
	for i, prefix := range []string{keySegment, runSegment, nameSegment, authorSegment} {
		if !strings.HasPrefix(segments[i], prefix) {
			return note.Note{}, fmt.Errorf("%w: object %q segment %q lacks the %q prefix", errNotANote, objName, segments[i], prefix)
		}
	}

	key, err := decodeSegment(segments[0], keySegment)
	if err != nil {
		return note.Note{}, fmt.Errorf("object %q: %w", objName, err)
	}
	run, err := strconv.Atoi(strings.TrimPrefix(segments[1], runSegment))
	if err != nil {
		return note.Note{}, fmt.Errorf("object %q: run segment %q is not a number: %w", objName, segments[1], err)
	}
	name, err := decodeSegment(segments[2], nameSegment)
	if err != nil {
		return note.Note{}, fmt.Errorf("object %q: %w", objName, err)
	}
	author, err := decodeSegment(segments[3], authorSegment)
	if err != nil {
		return note.Note{}, fmt.Errorf("object %q: %w", objName, err)
	}

	n := note.Note{Key: key, Run: run, Name: name, Author: author}
	if err := n.Validate(); err != nil {
		return note.Note{}, fmt.Errorf("object %q: %w", objName, err)
	}
	if canonical := objectName(root, key, run, name, author); canonical != objName {
		return note.Note{}, fmt.Errorf("object %q is not the canonical name for the coordinates it encodes (%q)", objName, canonical)
	}
	return n, nil
}

// decodeSegment strips a segment's role prefix and unescapes its value after
// parseObjectName has checked every role prefix. A malformed escape is fatal
// because the object has the note layout but its content is corrupt.
func decodeSegment(segment, prefix string) (string, error) {
	value, err := url.PathUnescape(strings.TrimPrefix(segment, prefix))
	if err != nil {
		return "", fmt.Errorf("segment %q is not a valid escaped value: %w", segment, err)
	}
	return value, nil
}

// listPrefix returns the longest object-name prefix that every note matching f
// must share. Coordinates narrow a prefix scan only in path order, so the prefix
// stops at f's first wildcard: a Filter with a Name but no Key yields the bare
// root, and the caller narrows the rest with [note.Filter.Matches].
//
// When f fixes all four coordinates the prefix is the note's full object name,
// which is a prefix of longer names too (Author "a" also prefixes "author_ab"),
// so the residual match is load-bearing even then. The shared scope is the case
// where that costs most: Author "" yields a bare "author_" prefix, which every
// author's segment starts with, so selecting the shared note alone still scans
// the whole fan and discards it. A caller that knows all four coordinates should
// Get the note rather than List for it.
func listPrefix(root string, f note.Filter) string {
	prefix := root
	if f.Key == "" {
		return prefix
	}
	prefix += keySegment + url.PathEscape(f.Key) + "/"
	if f.Run == nil {
		return prefix
	}
	prefix += runSegment + strconv.Itoa(*f.Run) + "/"
	if f.Name == "" {
		return prefix
	}
	prefix += nameSegment + url.PathEscape(f.Name) + "/"
	if f.Author == nil {
		return prefix
	}
	return prefix + authorSegment + url.PathEscape(*f.Author)
}
