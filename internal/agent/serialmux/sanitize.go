// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package serialmux provides the per-VM serial console multiplexer
// that reads from QEMU's single-consumer chardev socket and fans bytes
// out to one console subscriber (bidirectional) and any number of
// logs subscribers (read-only). See ADR 0029 for the architectural
// rationale.
package serialmux

// sanitize applies moderate sanitization to raw serial bytes per the
// L14 rules captured in the iteration spec:
//
//   - Strip control characters except \t (0x09) and \n (0x0A).
//   - Normalize CRLF (\r\n) and standalone CR to LF: \r is unconditionally
//     dropped, so a preceding \r before \n simply disappears.
//   - Preserve UTF-8 multi-byte sequences (any byte >= 0x80 passes
//     through unchanged; we do not validate UTF-8 - guest output may
//     legitimately contain non-UTF-8 noise).
//   - Preserve ANSI CSI escape sequences (ESC '[' parameters*
//     intermediates* final) verbatim when fully present in the chunk.
//   - Strip NUL (0x00), BEL (0x07), DEL (0x7F), and every other C0
//     control byte not whitelisted above.
//
// The function operates on a single chunk; an ANSI CSI sequence split
// across two calls will have its trailing fragment dropped. Spec §21
// flags cross-chunk continuity as a future buffering concern - the
// current implementation favours statelessness so the pump goroutine
// can call sanitize with zero coordination.
//
// The input slice is never modified. The returned slice is a fresh
// allocation (or an empty slice for empty input).
func sanitize(data []byte) []byte {
	out := make([]byte, 0, len(data))
	n := len(data)
	for i := 0; i < n; i++ {
		b := data[i]
		if b == 0x1B {
			emit, skip, incomplete := scanCSI(data, i)
			if incomplete {
				return out
			}
			if len(emit) > 0 {
				out = append(out, emit...)
			}
			// Loop's i++ supplies the +1; we add the rest.
			i += skip - 1
			continue
		}
		switch {
		case b == '\r':
			continue
		case b == '\t' || b == '\n':
			out = append(out, b)
		case b < 0x20, b == 0x7F:
			continue
		default:
			out = append(out, b)
		}
	}
	return out
}

// scanCSI examines data at offset i, where data[i] is the ESC byte
// (0x1B), and decides what to emit. Returns:
//
//   - emit: the CSI sequence bytes to copy through verbatim, or nil
//     when the ESC should be dropped (not a CSI sequence, or chunk
//     ended before CSI termination).
//   - skip: number of input bytes consumed from offset i.
//   - incomplete: true when the chunk ends mid-CSI; the caller drops
//     the remainder of the chunk.
//
// The CSI grammar (ECMA-48): ESC '[' parameters? intermediates? final
//
//	parameters    = 0x30-0x3F bytes (digits, ';', ':', '?', '<', '=', '>')
//	intermediates = 0x20-0x2F bytes
//	final         = 0x40-0x7E byte (single)
func scanCSI(data []byte, i int) (emit []byte, skip int, incomplete bool) {
	n := len(data)
	// ESC at the very end of the chunk - we cannot tell whether a
	// CSI introducer follows in a future call, so treat as incomplete.
	if i+1 >= n {
		return nil, n - i, true
	}
	if data[i+1] != '[' {
		// ESC not followed by '['; drop just the ESC byte and resume
		// normal sanitization on the next byte.
		return nil, 1, false
	}
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
