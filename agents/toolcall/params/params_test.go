/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package params_test

import (
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/params"
)

func TestExtract(t *testing.T) {
	args := map[string]any{
		"name":     "test",
		"count":    float64(42),
		"flag":     true,
		"bigcount": float64(9999999999),
		"empty":    "",
		"zero":     float64(0),
	}

	t.Run("string", func(t *testing.T) {
		v, err := params.Extract[string](args, "name")
		if err != nil {
			t.Fatal(err)
		}
		if v != "test" {
			t.Errorf("got %q, want %q", v, "test")
		}
	})

	t.Run("empty string", func(t *testing.T) {
		v, err := params.Extract[string](args, "empty")
		if err != nil {
			t.Fatal(err)
		}
		if v != "" {
			t.Errorf("got %q, want empty string", v)
		}
	})

	t.Run("int from float64", func(t *testing.T) {
		v, err := params.Extract[int](args, "count")
		if err != nil {
			t.Fatal(err)
		}
		if v != 42 {
			t.Errorf("got %d, want 42", v)
		}
	})

	t.Run("int32 from float64", func(t *testing.T) {
		v, err := params.Extract[int32](args, "count")
		if err != nil {
			t.Fatal(err)
		}
		if v != 42 {
			t.Errorf("got %d, want 42", v)
		}
	})

	t.Run("int64 from float64", func(t *testing.T) {
		v, err := params.Extract[int64](args, "bigcount")
		if err != nil {
			t.Fatal(err)
		}
		if v != 9999999999 {
			t.Errorf("got %d, want 9999999999", v)
		}
	})

	t.Run("float64", func(t *testing.T) {
		v, err := params.Extract[float64](args, "count")
		if err != nil {
			t.Fatal(err)
		}
		if v != 42 {
			t.Errorf("got %f, want 42", v)
		}
	})

	t.Run("bool", func(t *testing.T) {
		v, err := params.Extract[bool](args, "flag")
		if err != nil {
			t.Fatal(err)
		}
		if !v {
			t.Error("got false, want true")
		}
	})

	t.Run("zero int", func(t *testing.T) {
		v, err := params.Extract[int](args, "zero")
		if err != nil {
			t.Fatal(err)
		}
		if v != 0 {
			t.Errorf("got %d, want 0", v)
		}
	})

	t.Run("missing", func(t *testing.T) {
		_, err := params.Extract[string](args, "missing")
		if err == nil {
			t.Fatal("expected error for missing parameter")
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		_, err := params.Extract[bool](args, "name")
		if err == nil {
			t.Fatal("expected error for wrong type")
		}
	})
}

func TestExtractOptional(t *testing.T) {
	args := map[string]any{
		"name":  "test",
		"count": float64(42),
	}

	t.Run("present", func(t *testing.T) {
		v, err := params.ExtractOptional(args, "name", "default")
		if err != nil {
			t.Fatal(err)
		}
		if v != "test" {
			t.Errorf("got %q, want %q", v, "test")
		}
	})

	t.Run("missing uses default", func(t *testing.T) {
		v, err := params.ExtractOptional(args, "missing", "default")
		if err != nil {
			t.Fatal(err)
		}
		if v != "default" {
			t.Errorf("got %q, want %q", v, "default")
		}
	})

	t.Run("int conversion", func(t *testing.T) {
		v, err := params.ExtractOptional(args, "count", 0)
		if err != nil {
			t.Fatal(err)
		}
		if v != 42 {
			t.Errorf("got %d, want 42", v)
		}
	})

	t.Run("type mismatch", func(t *testing.T) {
		_, err := params.ExtractOptional(args, "name", 0)
		if err == nil {
			t.Fatal("expected error for type mismatch")
		}
	})
}

func TestError(t *testing.T) {
	result := params.Error("bad %s", "input")
	if result["error"] != "bad input" {
		t.Errorf("got %q, want %q", result["error"], "bad input")
	}
}

func TestErrorWithContext(t *testing.T) {
	result := params.ErrorWithContext(errors.New("failed"), map[string]any{"path": "/foo"})
	if result["error"] != "failed" {
		t.Errorf("got %q, want %q", result["error"], "failed")
	}
	if result["path"] != "/foo" {
		t.Errorf("got %q, want %q", result["path"], "/foo")
	}
}

func TestErrorWithContext_NilContext(t *testing.T) {
	result := params.ErrorWithContext(errors.New("failed"), nil)
	if result["error"] != "failed" {
		t.Errorf("got %q, want %q", result["error"], "failed")
	}
}
