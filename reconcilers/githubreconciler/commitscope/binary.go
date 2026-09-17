/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import (
	"bytes"
	"errors"
	"io"
)

// sniffLimit is how many leading bytes the binary sniff reads. Git uses the
// first 8000 bytes for its own text/binary diff heuristic.
const sniffLimit = 8000

// binaryMagics are leading byte sequences of common executable formats. The NUL
// scan catches most binaries; these catch formats whose first bytes are all
// non-NUL (a Mach-O universal binary, a PE header).
var binaryMagics = [][]byte{
	{0x7f, 'E', 'L', 'F'},    // ELF
	{'M', 'Z'},               // PE / DOS
	{0xfe, 0xed, 0xfa, 0xce}, // Mach-O 32-bit
	{0xfe, 0xed, 0xfa, 0xcf}, // Mach-O 64-bit
	{0xce, 0xfa, 0xed, 0xfe}, // Mach-O 32-bit, reversed
	{0xcf, 0xfa, 0xed, 0xfe}, // Mach-O 64-bit, reversed
	{0xca, 0xfe, 0xba, 0xbe}, // Mach-O universal / Java class
	{0xbe, 0xba, 0xfe, 0xca}, // Mach-O universal, reversed
}

// sniffBinary reports whether the first bytes read from r look like binary
// content: a NUL byte within the sniff window, or a known executable magic.
func sniffBinary(r io.Reader) (bool, error) {
	buf := make([]byte, sniffLimit)
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	return looksBinary(buf[:n]), nil
}

// looksBinary applies the sniff rules to an already-read prefix.
func looksBinary(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	for _, magic := range binaryMagics {
		if bytes.HasPrefix(b, magic) {
			return true
		}
	}
	return false
}
