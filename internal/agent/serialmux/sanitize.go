// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package serialmux provides the per-VM serial console multiplexer
// that reads from QEMU's single-consumer chardev socket and fans bytes
// out to one console subscriber (bidirectional) and any number of
// logs subscribers (read-only).
package serialmux

// maxPendingCarry caps the partial-CSI bytes held between Process
// calls. A real CSI sequence is at most ~10 bytes; 64 gives us
// generous slack for SS3 / OSC variations we may add later, while
// still bounding worst-case memory growth if the guest emits a
// long sequence of bare ESC bytes that never terminates.
const maxPendingCarry = 64

// Sanitizer is the stateful wrapper around the byte-level sanitize
// rules in sanitizeWithCarry. The multiplexer instantiates one per
// VM and feeds every chunk read from the QEMU socket through Process;
// the Sanitizer carries partial ANSI CSI sequences across chunk
// boundaries so the L14 "preserve ANSI CSI verbatim" rule is honored
// even when the QEMU socket Read happens to split mid-sequence.
//
// Sanitizer is not safe for concurrent use; the pump is the sole
// caller and serialises chunk processing by construction.
type Sanitizer struct {
	pending []byte
}

// NewSanitizer returns a fresh sanitizer with an empty carry buffer.
func NewSanitizer() *Sanitizer { return &Sanitizer{} }

// Process sanitizes the next chunk of raw serial bytes. It merges
// any pending partial CSI bytes from the previous Process call,
// applies the L14 sanitization rules, and saves any new trailing
// partial CSI bytes into the carry buffer for the next call. The
// returned slice is a fresh allocation.
func (s *Sanitizer) Process(chunk []byte) []byte {
	var data []byte
	if len(s.pending) > 0 {
		data = make([]byte, 0, len(s.pending)+len(chunk))
		data = append(data, s.pending...)
		data = append(data, chunk...)
		s.pending = nil
	} else {
		data = chunk
	}
	out, carry := sanitizeWithCarry(data)
	if len(carry) > 0 {
		if len(carry) > maxPendingCarry {
			// Pending grew past the cap; the carry is almost
			// certainly garbage (orphan ESC bytes never terminated).
			// Drop it so the next chunk starts clean.
			return out
		}
		s.pending = append([]byte(nil), carry...)
	}
	return out
}

// sanitizeWithCarry applies the L14 sanitization rules to data and
// returns (out, carry):
//
//   - out: sanitized bytes ready for emission.
//   - carry: trailing bytes that may be the head of an ANSI escape
//     sequence whose tail is in the next chunk; the caller
//     (Sanitizer) saves them and prepends to the next Process call.
//
// L14 rules (revised after real-agent smoke caught the staircase
// effect that stripping CR produced on console-stream sessions):
//
//   - Strip C0 control bytes except \b (0x08), \t (0x09), \n (0x0A),
//     and \r (0x0D). CR is preserved so a terminal in raw mode (the
//     interactive console CLI) renders `\r\n` line endings as "col 0,
//     next row" and so progress bars / spinner output that overwrites
//     the current line with `\r[ * ]\r[ ** ]` keeps working both live
//     and replayed via the on-disk log file. BS is preserved so the
//     guest's line-editing echo reaches the interactive console: bash
//     readline moves the cursor left with `\b` and erases a character
//     with the `\b \b` (backspace, space, backspace) sequence. Stripping
//     `\b` left the keystroke working (it is sent to the guest on the
//     un-sanitized input path) while the visual echo silently vanished -
//     backspace and the left/right arrow keys edited the line but never
//     moved the cursor on screen.
//   - Preserve UTF-8 multi-byte sequences (bytes >= 0x80 pass through
//     unchanged; we do not validate UTF-8).
//   - Preserve ANSI CSI escape sequences (ESC '[' parameters?
//     intermediates? final) verbatim. A CSI that runs off the end of
//     the chunk surfaces in carry so the caller can stitch it back
//     together on the next call.
//   - Drop non-CSI ESC sequences wholesale - charset designation,
//     RI / IND / NEL single-byte controls, DEC private modes - so
//     their trailing printable byte (e.g. the `M` of `\x1bM`) does
//     not leak into the output as if it were normal text.
//   - Strip NUL (0x00), BEL (0x07), DEL (0x7F), and every other C0
//     control byte not whitelisted above.
//
// The input slice is never modified.
func sanitizeWithCarry(data []byte) (out, carry []byte) {
	out = make([]byte, 0, len(data))
	n := len(data)
	for i := 0; i < n; i++ {
		b := data[i]
		if b == 0x1B {
			emit, skip, incomplete := scanCSI(data, i)
			if incomplete {
				// Save the partial CSI (ESC plus whatever follows
				// in this chunk) so the next call can resume scan
				// with the rest of the sequence.
				carry = data[i:n]
				return out, carry
			}
			if len(emit) > 0 {
				out = append(out, emit...)
			}
			// Loop's i++ supplies the +1; we add the rest.
			i += skip - 1
			continue
		}
		switch {
		case b == '\b' || b == '\t' || b == '\n' || b == '\r':
			out = append(out, b)
		case b < 0x20, b == 0x7F:
			continue
		default:
			out = append(out, b)
		}
	}
	return out, nil
}

// sanitize is the stateless single-chunk entry point retained for the
// existing TestSanitize table and any caller that genuinely wants
// per-chunk semantics (cross-chunk CSI splits drop). The production
// pump uses Sanitizer.Process instead.
func sanitize(data []byte) []byte {
	out, _ := sanitizeWithCarry(data)
	return out
}

// scanCSI examines data at offset i, where data[i] is the ESC byte
// (0x1B), and decides what to emit. Returns:
//
//   - emit: the CSI sequence bytes to copy through verbatim, or nil
//     when the entire escape sequence should be dropped (a non-CSI
//     escape, a chunk that ends mid-sequence, etc.).
//   - skip: number of input bytes consumed from offset i.
//   - incomplete: true when the chunk ends mid-sequence; the caller
//     saves the remainder in the carry buffer for the next chunk.
//
// L14 preserves CSI sequences. The other ANSI / VT-style escapes
// (charset designation, RI / IND / NEL single-byte controls, DEC
// private modes) are dropped wholesale - they carry no visible
// glyph but their bytes after ESC are in the printable ASCII range
// and would otherwise leak as literal text. The famous example is
// systemd-networkd's `\x1bM` reverse-index emitted when a progress
// line collapses; before this carve-out the trailing `M` surfaced
// in the log right before the `[  OK  ]` marker.
//
// The grammars handled:
//
//   - CSI:       ESC '[' parameters? intermediates? final     -> preserve
//     parameters    = 0x30-0x3F bytes (digits, ';', ':', '?', '<', '=', '>')
//     intermediates = 0x20-0x2F bytes
//     final         = 0x40-0x7E byte (single)
//   - 3-byte:    ESC ('(' | ')' | '*' | '+' | '-' | '.' | '/' | '#' | '%') X
//     Charset designation (ISO 2022) and DEC private 3-byte forms.
//   - 2-byte:    ESC X for any other byte after ESC (Fe controls,
//     simple 2-byte escapes).
func scanCSI(data []byte, i int) (emit []byte, skip int, incomplete bool) {
	n := len(data)
	// ESC at the very end of the chunk - we cannot tell whether a
	// CSI introducer follows in a future call, so treat as incomplete.
	if i+1 >= n {
		return nil, n - i, true
	}
	second := data[i+1]
	if second == '[' {
		k := i + 2
		for k < n && data[k] >= 0x30 && data[k] <= 0x3F {
			k++
		}
		for k < n && data[k] >= 0x20 && data[k] <= 0x2F {
			k++
		}
		if k < n && data[k] >= 0x40 && data[k] <= 0x7E {
			return data[i : k+1], (k + 1) - i, false
		}
		return nil, n - i, true
	}
	// 3-byte ESC sequences (charset designation, DEC private). Need
	// the byte at i+2 to know the full length; if absent, carry the
	// 2 bytes we have to the next chunk.
	if isThreeByteEscIntroducer(second) {
		if i+2 >= n {
			return nil, n - i, true
		}
		return nil, 3, false
	}
	// 2-byte ESC sequence: ESC + a single Fe control or similar. Drop
	// both bytes so the second byte does not leak into the output
	// as if it were a printable character.
	return nil, 2, false
}

func isThreeByteEscIntroducer(b byte) bool {
	switch b {
	case '(', ')', '*', '+', '-', '.', '/', '#', '%':
		return true
	}
	return false
}
