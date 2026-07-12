/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package schema_test

import (
	"context"
	"encoding/json"
	"fmt"

	"chainguard.dev/driftlessaf/agents/schema"
)

// ExampleReflectType demonstrates generating a JSON schema from a Go type.
func ExampleReflectType() {
	type Input struct {
		Path   string `json:"path" jsonschema:"required,description=File path to read"`
		Offset int    `json:"offset,omitempty" jsonschema:"description=Byte offset"`
	}

	s := schema.ReflectType[Input]()
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		panic(err)
	}
	fmt.Println("type:", m["type"])
	// Output: type: object
}

// ExampleNewGenerator demonstrates creating a generator and reflecting a value.
func ExampleNewGenerator() {
	type Params struct {
		Query string `json:"query" jsonschema:"required"`
	}

	g := schema.NewGenerator()
	s := g.Reflect(&Params{})
	data, _ := json.Marshal(s)

	var m map[string]any
	_ = json.Unmarshal(data, &m)
	fmt.Println("type:", m["type"])
	// Output: type: object
}

// ExampleValidate demonstrates checking a decoded JSON document against a
// reflected schema.
func ExampleValidate() {
	type Verdict struct {
		Label      string  `json:"label" jsonschema:"required,enum=benign,enum=malicious"`
		Confidence float64 `json:"confidence" jsonschema:"minimum=0,maximum=1"`
	}

	var doc any
	_ = json.Unmarshal([]byte(`{"label":"unsure","confidence":1.5}`), &doc)

	for _, v := range schema.Validate(schema.ReflectType[Verdict](), doc) {
		fmt.Println(v)
	}
	// Output:
	// label: value "unsure" is not one of the allowed values ["benign", "malicious"]
	// confidence: value 1.5 exceeds the maximum 1
}

// ExampleResultValidator demonstrates the schema-conformance validator the
// executors install as the base of every submit tool's validator chain.
func ExampleResultValidator() {
	type Verdict struct {
		Label      string  `json:"label" jsonschema:"required,enum=benign,enum=malicious"`
		Confidence float64 `json:"confidence" jsonschema:"minimum=0,maximum=1"`
	}

	validate := schema.ResultValidator[*Verdict]()
	findings, _ := validate(context.Background(), &Verdict{Label: "unsure", Confidence: 0.5}, "reasoning")
	for _, f := range findings {
		fmt.Println(f.Identifier)
	}
	// Output:
	// schema:label
}
