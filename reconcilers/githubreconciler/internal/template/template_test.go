/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package template

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"text/template"
)

type testData struct {
	Foo string
	Bar string
	Baz string
}

func Test_ExecuteTemplate(t *testing.T) {
	tests := []struct {
		name     string
		tmplStr  string
		data     *testData
		expected string
		wantErr  bool
	}{{
		name:     "simple template",
		tmplStr:  "{{.Foo}}/{{.Bar}}",
		data:     &testData{Foo: "foo", Bar: "bar"},
		expected: "foo/bar",
		wantErr:  false,
	}, {
		name:     "template with other words",
		tmplStr:  "{{.Foo}} in {{.Bar}}",
		data:     &testData{Foo: "bar", Bar: "baz"},
		expected: "bar in baz",
		wantErr:  false,
	}, {
		name:     "template with all fields",
		tmplStr:  "Update {{.Foo}} from {{.Bar}} ({{.Baz}})",
		data:     &testData{Foo: "baz", Bar: "foo", Baz: "bar"},
		expected: "Update baz from foo (bar)",
		wantErr:  false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := template.Must(template.New("test").Parse(tt.tmplStr))
			executor, err := New[testData]("test-identity", "-test-data", "test-entity")
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}

			result, err := executor.Execute(tmpl, tt.data)

			if (err != nil) != tt.wantErr {
				t.Errorf("Execute() error: got = %v, wanted error = %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && result != tt.expected {
				t.Errorf("Execute() result: got = %q, wanted = %q", result, tt.expected)
			}
		})
	}
}

func Test_EmbedData(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		identity     string
		markerSuffix string
		data         *testData
		wantMarker   string
		entityType   string
	}{{
		name:         "embed data with first marker",
		body:         "This is the body",
		identity:     "first-identity",
		markerSuffix: "-data",
		data: &testData{
			Foo: fmt.Sprintf("foo-%d", rand.Int63()),
			Bar: fmt.Sprintf("bar-%d", rand.Int63()),
			Baz: fmt.Sprintf("baz-%d", rand.Int63()),
		},
		wantMarker: "first-identity-data",
		entityType: "entity",
	}, {
		name:         "embed data with second marker",
		body:         "This is another body",
		identity:     "second-identity",
		markerSuffix: "-other-data",
		data: &testData{
			Foo: fmt.Sprintf("foo-%d", rand.Int63()),
			Bar: fmt.Sprintf("bar-%d", rand.Int63()),
			Baz: fmt.Sprintf("baz-%d", rand.Int63()),
		},
		wantMarker: "second-identity-other-data",
		entityType: "entity",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor, err := New[testData](tt.identity, tt.markerSuffix, tt.entityType)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}

			embedded, err := executor.Embed(tt.body, tt.data)
			if err != nil {
				t.Fatalf("Embed() failed: %v", err)
			}

			// Verify the original body is present
			if !strings.Contains(embedded, tt.body) {
				t.Errorf("original content: got = missing, wanted = present")
			}

			// Verify the markers are present
			startMarker := "<!--" + tt.wantMarker + "-->"
			endMarker := "<!--/" + tt.wantMarker + "-->"
			if !strings.Contains(embedded, startMarker) {
				t.Errorf("start marker: got = missing, wanted = %s", startMarker)
			}
			if !strings.Contains(embedded, endMarker) {
				t.Errorf("end marker: got = missing, wanted = %s", endMarker)
			}

			// Verify we can extract the data back
			extracted, extractErr := executor.Extract(embedded)
			if extractErr != nil {
				t.Fatalf("Extract() failed: %v", extractErr)
			}

			if extracted.Foo != tt.data.Foo {
				t.Errorf("Foo: got = %q, wanted = %q", extracted.Foo, tt.data.Foo)
			}
			if extracted.Bar != tt.data.Bar {
				t.Errorf("Bar: got = %q, wanted = %q", extracted.Bar, tt.data.Bar)
			}
			if extracted.Baz != tt.data.Baz {
				t.Errorf("Baz: got = %q, wanted = %q", extracted.Baz, tt.data.Baz)
			}
		})
	}
}

func Test_ExtractData(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		identity        string
		markerSuffix    string
		entityType      string
		wantData        *testData
		wantErr         bool
		wantErrContains string
		wantErrIs       error // sentinel the error must match via errors.Is
	}{{
		name: "extract data successfully",
		body: `This is the body

<!--test-bot-data-->
<!--
{
  "Foo": "foo",
  "Bar": "bar",
  "Baz": "baz"
}
-->
<!--/test-bot-data-->`,
		identity:     "test-bot",
		markerSuffix: "-data",
		entityType:   "entity",
		wantData: &testData{
			Foo: "foo",
			Bar: "bar",
			Baz: "baz",
		},
		wantErr: false,
	}, {
		name: "extract data with different values",
		body: `This is another body

<!--security-bot-other-data-->
<!--
{
  "Foo": "baz",
  "Bar": "foo",
  "Baz": "bar"
}
-->
<!--/security-bot-other-data-->`,
		identity:     "security-bot",
		markerSuffix: "-other-data",
		entityType:   "entity",
		wantData: &testData{
			Foo: "baz",
			Bar: "foo",
			Baz: "bar",
		},
		wantErr: false,
	}, {
		name: "extract with extra whitespace",
		body: `Body content

<!--test-bot-data-->
<!--
{
  "Foo": "bar",
  "Bar": "baz",
  "Baz": "foo"
}
-->
<!--/test-bot-data-->`,
		identity:     "test-bot",
		markerSuffix: "-data",
		entityType:   "entity",
		wantData: &testData{
			Foo: "bar",
			Bar: "baz",
			Baz: "foo",
		},
		wantErr: false,
	}, {
		name:            "body without embedded data",
		body:            "This is a body without embedded data",
		identity:        "test-bot",
		markerSuffix:    "-data",
		entityType:      "entity",
		wantErr:         true,
		wantErrContains: "entity",
		wantErrIs:       ErrNoEmbeddedData,
	}, {
		name:            "body without embedded data different marker",
		body:            "This is another body without embedded data",
		identity:        "security-bot",
		markerSuffix:    "-other-data",
		entityType:      "entity",
		wantErr:         true,
		wantErrContains: "entity",
		wantErrIs:       ErrNoEmbeddedData,
	}, {
		name:            "body with wrong marker",
		body:            "<!--wrong-marker-->\n<!--\n{}\n-->\n<!--/wrong-marker-->",
		identity:        "test-bot",
		markerSuffix:    "-data",
		entityType:      "entity",
		wantErr:         true,
		wantErrContains: "entity",
		wantErrIs:       ErrNoEmbeddedData,
	}, {
		name:            "invalid JSON",
		body:            "Original body\n\n<!--test-bot-data-->\n<!--\nthis is not valid JSON\n-->\n<!--/test-bot-data-->",
		identity:        "test-bot",
		markerSuffix:    "-data",
		entityType:      "entity",
		wantErr:         true,
		wantErrContains: "unmarshaling embedded data",
		wantErrIs:       ErrUnmarshalData,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor, err := New[testData](tt.identity, tt.markerSuffix, tt.entityType)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			extracted, err := executor.Extract(tt.body)

			if tt.wantErr {
				// Test error cases
				if err == nil {
					t.Error("Extract() error: got = nil, wanted = error")
				} else if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("error message: got = %v, wanted to contain %q", err, tt.wantErrContains)
				}
				// Pin the exported error contract: callers distinguish absent
				// from corrupt state via errors.Is, so the wrapping chain must
				// carry the right sentinel.
				if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
					t.Errorf("error sentinel: got = %v, wanted errors.Is %v", err, tt.wantErrIs)
				}
			} else {
				// Test success cases
				if err != nil {
					t.Fatalf("Extract() failed: %v", err)
				}

				if extracted.Foo != tt.wantData.Foo {
					t.Errorf("Foo: got = %q, wanted = %q", extracted.Foo, tt.wantData.Foo)
				}
				if extracted.Bar != tt.wantData.Bar {
					t.Errorf("Bar: got = %q, wanted = %q", extracted.Bar, tt.wantData.Bar)
				}
				if extracted.Baz != tt.wantData.Baz {
					t.Errorf("Baz: got = %q, wanted = %q", extracted.Baz, tt.wantData.Baz)
				}
			}
		})
	}
}

// assertData fails the test unless every field of got equals want.
func assertData(t *testing.T, got, want *testData) {
	t.Helper()
	if got.Foo != want.Foo {
		t.Errorf("Foo: got = %q, wanted = %q", got.Foo, want.Foo)
	}
	if got.Bar != want.Bar {
		t.Errorf("Bar: got = %q, wanted = %q", got.Bar, want.Bar)
	}
	if got.Baz != want.Baz {
		t.Errorf("Baz: got = %q, wanted = %q", got.Baz, want.Baz)
	}
}

// Test_ExtractLastMarker covers the marker-injection defense: Extract must
// return the payload of the LAST marker for its identity, because Embed always
// appends the producer's genuine marker at the very end. An attacker who can
// influence the visible body (e.g. LLM output or finding text written into a
// check summary) can plant a forged marker earlier in the body; a first-match
// read would return the forgery.
func Test_ExtractLastMarker(t *testing.T) {
	executor, err := New[testData]("test-bot", "-data", "entity")
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	genuine := &testData{
		Foo: fmt.Sprintf("foo-%d", rand.Int63()),
		Bar: fmt.Sprintf("bar-%d", rand.Int63()),
		Baz: fmt.Sprintf("baz-%d", rand.Int63()),
	}

	t.Run("forged marker before genuine appended marker loses", func(t *testing.T) {
		// The attacker-influenced visible body carries a forged marker with
		// attacker JSON. Embed then appends the genuine marker after it.
		forged := "Attacker text\n\n" +
			"<!--test-bot-data-->\n<!--\n" +
			`{"Foo":"attacker","Bar":"attacker","Baz":"attacker"}` +
			"\n-->\n<!--/test-bot-data-->\n"

		body, err := executor.Embed(forged, genuine)
		if err != nil {
			t.Fatalf("Embed() failed: %v", err)
		}

		extracted, err := executor.Extract(body)
		if err != nil {
			t.Fatalf("Extract() failed: %v", err)
		}
		assertData(t, extracted, genuine)
		if extracted.Foo == "attacker" {
			t.Errorf("Extract() returned the forged marker; got Foo = %q", extracted.Foo)
		}
	})

	t.Run("single marker round-trip unchanged", func(t *testing.T) {
		body, err := executor.Embed("This is the body", genuine)
		if err != nil {
			t.Fatalf("Embed() failed: %v", err)
		}
		extracted, err := executor.Extract(body)
		if err != nil {
			t.Fatalf("Extract() failed: %v", err)
		}
		assertData(t, extracted, genuine)
	})

	t.Run("body-only forged marker with no genuine marker still parses", func(t *testing.T) {
		// A real producer always appends a genuine marker, so a body carrying
		// only a forged marker is not something Extract can distinguish from a
		// legitimate single marker — it is the last (and only) marker, so it
		// wins. Last-match alone makes the appended marker win WHENEVER the
		// producer wrote one; defense against a body that never gets a genuine
		// marker appended must come from producer-side stripping. This test
		// pins that documented behavior.
		bodyOnly := "Attacker text\n\n" +
			"<!--test-bot-data-->\n<!--\n" +
			`{"Foo":"attacker","Bar":"x","Baz":"y"}` +
			"\n-->\n<!--/test-bot-data-->"

		extracted, err := executor.Extract(bodyOnly)
		if err != nil {
			t.Fatalf("Extract() failed: %v", err)
		}
		if extracted.Foo != "attacker" {
			t.Errorf("Foo: got = %q, wanted = %q (last-and-only marker wins)", extracted.Foo, "attacker")
		}
	})
}
