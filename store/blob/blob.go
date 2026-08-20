/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package blob

import "errors"

// Gen is a blob object's generation — a version stamp advanced on every write,
// used as the compare-and-swap token. The zero Gen never matches a stored
// object, so it can be passed wherever "no generation" is meant.
type Gen int64

// Cond is a precondition applied to a Put.
//
// The zero Cond is unconditional: the write always lands, overwriting any
// existing object. Set one field to make the write conditional.
type Cond struct {
	// DoesNotExist requires the object be absent — a create-only write. A Put
	// with DoesNotExist against an existing object fails with
	// ErrPreconditionFailed, which is how idempotent, content-addressed callers
	// detect "already stored".
	DoesNotExist bool

	// GenerationMatch requires the object's current generation equal this value
	// — a read-modify-write. It is ignored when zero, and when DoesNotExist is
	// set (DoesNotExist takes precedence).
	GenerationMatch Gen
}

// Object is one entry returned by List: an object's full name and current
// generation. List reports names and generations only, never bodies.
type Object struct {
	Name string
	Gen  Gen
}

// Page is one bounded slice of a List plus the cursor for the next. List never
// returns more than its limit, so a listing over a wide prefix stays bounded.
type Page struct {
	Objects []Object
	// Cursor continues the listing: pass it back to List (with the same prefix)
	// to fetch the next page. It is "" when the listing is exhausted, and is
	// opaque and backend-specific — only replay it against the backend and
	// prefix that produced it.
	Cursor string
}

// Sentinel errors that every backend maps its native failures onto, so callers
// can branch on them with errors.Is regardless of which backend is in use.
var (
	// ErrNotExist is returned by an unconditional Delete (ifGen == 0) of an
	// absent object. (Get reports absence through its ok result rather than this
	// error.)
	ErrNotExist = errors.New("blob: object does not exist")

	// ErrPreconditionFailed is returned when a Put or Delete precondition is not
	// satisfied: a DoesNotExist against an existing object, or a GenerationMatch
	// that doesn't match — including a generation-conditional Delete or Put
	// against an absent object, where the generation can never match.
	ErrPreconditionFailed = errors.New("blob: precondition failed")
)
