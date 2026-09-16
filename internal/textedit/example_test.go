/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package textedit_test

import (
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/internal/textedit"
)

func ExampleMatchIgnoringIndentation() {
	file := "func f() {\n\tif ok {\n\t\treturn 1\n\t}\n}\n"

	// The agent copied the block with one tab too few on every line.
	old := "if ok {\n\treturn 1\n}"

	m, err := textedit.MatchIgnoringIndentation(strings.NewReader(file), old)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("line %d, bytes [%d, %d), %s\n", m.Line, m.Start, m.End, m.Shift)
	fmt.Printf("%q\n", file[m.Start:m.End])
	// Output:
	// line 2, bytes [11, 33), added 1 tab
	// "\tif ok {\n\t\treturn 1\n\t}"
}

func ExampleMatchIgnoringIndentation_ambiguous() {
	file := "\tx()\n\ty()\n---\n\t\tx()\n\t\ty()\n"

	_, err := textedit.MatchIgnoringIndentation(strings.NewReader(file), "x()\ny()")
	fmt.Println(err)
	// Output:
	// old_string not found in file: ignoring indentation it matches at least 2 regions (byte offsets 0 and 14); include more surrounding context to make it unique
}

func ExampleShiftIndentation() {
	replacement := "if ok {\n\treturn 2\n}\n"

	shifted := textedit.ShiftIndentation(replacement, textedit.Shift{Add: true, Whitespace: "\t"})
	fmt.Printf("%q\n", shifted)

	restored := textedit.ShiftIndentation(shifted, textedit.Shift{Whitespace: "\t"})
	fmt.Println(restored == replacement)
	// Output:
	// "\tif ok {\n\t\treturn 2\n\t}\n"
	// true
}

func ExampleShift_String() {
	fmt.Printf("%q\n", textedit.Shift{}.String())
	fmt.Println(textedit.Shift{Add: true, Whitespace: "\t"})
	fmt.Println(textedit.Shift{Whitespace: "    "})
	fmt.Println(textedit.Shift{Add: true, Whitespace: " \t"})
	// Output:
	// ""
	// added 1 tab
	// removed 4 spaces
	// added " \t"
}

func ExampleLineStart() {
	r := strings.NewReader("first line\nsecond line\nthird line\n")

	// A window that would begin mid-way through "second line" is moved back
	// to the start of that line.
	start, err := textedit.LineStart(r, 15, 64<<10)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(start)
	// Output:
	// 11
}

func ExampleLineEnd() {
	data := "first line\nsecond line\nthird line\n"
	r := strings.NewReader(data)

	// A window that would end mid-way through "second line" is extended to
	// include the rest of the line and its newline.
	end, err := textedit.LineEnd(r, 15, int64(len(data)), 64<<10)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(end)
	// Output:
	// 23
}
