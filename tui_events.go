package main

import (
	"strconv"
	"strings"
	"time"
)

// escapeSequenceDelay is how long the decoder waits after a lone ESC (or an
// incomplete escape sequence) before deciding no more bytes are coming.
// Terminals emit multi-byte escape sequences (arrow keys, SGR mouse reports)
// in a single write, but that write can still arrive across multiple reads
// when it crosses a pipe/pty buffer boundary — so a short byte-by-byte
// sequence is ambiguous between "user pressed Esc" and "sequence still
// arriving" until either more bytes show up or this delay elapses.
const escapeSequenceDelay = 20 * time.Millisecond

// Extended key constants returned by inputDecoder, alongside the existing
// KeyUp / KeyDown / KeyLeft / KeyRight / KeyEsc sentinels defined in tui.go.
const (
	KeyEnter    = "\x00enter"
	KeyHome     = "\x00home"
	KeyEnd      = "\x00end"
	KeyPageUp   = "\x00page-up"
	KeyPageDown = "\x00page-down"
)

// fixedSequences maps complete escape sequences to their key constant.
// Includes both CSI ("\x1b[") and SS3 ("\x1bO") forms since terminals differ
// on which they emit for arrows/Home/End depending on application-cursor-key
// mode, plus the VT220-style "\x1b[<n>~" forms for Home/End/PageUp/PageDown.
var fixedSequences = map[string]string{
	"\x1b[A": KeyUp, "\x1b[B": KeyDown,
	"\x1b[C": KeyRight, "\x1b[D": KeyLeft,
	"\x1bOA": KeyUp, "\x1bOB": KeyDown,
	"\x1bOC": KeyRight, "\x1bOD": KeyLeft,
	"\x1b[H": KeyHome, "\x1b[F": KeyEnd,
	"\x1bOH": KeyHome, "\x1bOF": KeyEnd,
	"\x1b[1~": KeyHome, "\x1b[4~": KeyEnd,
	"\x1b[5~": KeyPageUp, "\x1b[6~": KeyPageDown,
}

// eventKind distinguishes the payload carried by an inputEvent.
type eventKind uint8

const (
	eventKey eventKind = iota
	eventMouse
)

// mouseButton identifies which button (or wheel direction) an SGR mouse
// report refers to.
type mouseButton uint8

const (
	mouseLeft mouseButton = iota
	mouseMiddle
	mouseRight
	mouseRelease
	mouseWheelUp
	mouseWheelDown
)

// mouseEvent is a decoded SGR mouse report. x and y are zero-based
// (terminals report 1-based coordinates).
type mouseEvent struct {
	x, y    int
	button  mouseButton
	release bool
}

// inputEvent is a single decoded keystroke or mouse action.
type inputEvent struct {
	kind  eventKind
	key   string
	mouse mouseEvent
}

// inputDecoder turns a stream of raw terminal input bytes into inputEvents.
// It is stateful across Feed calls so that escape sequences split across
// separate reads (a common occurrence over ssh/tmux/slow ptys) still decode
// correctly instead of leaking a bare ESC followed by garbage keys.
type inputDecoder struct {
	buf          []byte
	pendingSince time.Time
}

// newInputDecoder returns a ready-to-use decoder with no buffered input.
func newInputDecoder() *inputDecoder { return &inputDecoder{} }

// Feed appends chunk to the decoder's buffer and extracts as many complete
// events as possible, leaving any trailing incomplete escape sequence
// buffered for the next Feed or Flush call. now is used to track how long an
// incomplete sequence has been pending.
func (d *inputDecoder) Feed(chunk []byte, now time.Time) []inputEvent {
	if len(chunk) > 0 {
		d.buf = append(d.buf, chunk...)
	}
	var out []inputEvent
	for len(d.buf) > 0 {
		ev, n, complete := d.decodeOne()
		if !complete {
			if len(d.buf) > 0 && d.buf[0] == 0x1b && d.pendingSince.IsZero() {
				d.pendingSince = now
			}
			break
		}
		if n == 0 {
			// Should not happen, but avoid an infinite loop if it does.
			break
		}
		d.buf = d.buf[n:]
		d.pendingSince = time.Time{}
		if ev != nil {
			out = append(out, *ev)
		}
	}
	return out
}

// decodeOne attempts to decode a single event from the front of d.buf.
// Returns the decoded event (nil for a consumed-but-eventless prefix, which
// does not currently occur but keeps the shape symmetric), the number of
// bytes consumed, and whether a complete decode was possible. When complete
// is false, n is meaningless and the caller should stop and wait for more
// input.
func (d *inputDecoder) decodeOne() (ev *inputEvent, n int, complete bool) {
	buf := d.buf
	b0 := buf[0]

	if b0 == '\r' || b0 == '\n' {
		return &inputEvent{kind: eventKey, key: KeyEnter}, 1, true
	}

	if b0 != 0x1b {
		return &inputEvent{kind: eventKey, key: string(rune(b0))}, 1, true
	}

	// From here, buf[0] == ESC. A lone trailing ESC (nothing after it yet)
	// is ambiguous: either the user pressed Esc, or a sequence is still
	// arriving. Report incomplete so Feed/Flush can decide based on timing.
	if len(buf) < 2 {
		return nil, 0, false
	}

	// SGR mouse report: ESC [ < ...
	if buf[1] == '[' && len(buf) >= 3 && buf[2] == '<' {
		return d.decodeSGRMouse()
	}

	// CSI ("\x1b[...") sequence. Per ECMA-48, the body after "ESC [" is
	// parameter bytes (0x30-0x3F) then intermediate bytes (0x20-0x2F),
	// terminated by exactly one final byte (0x40-0x7E); that range split
	// covers all of 0x20-0x7E, so scanning for the first byte >= 0x40
	// finds the true end of the sequence. Scanning to the real final byte
	// (rather than stopping at the first non-digit) is what lets modified
	// sequences like "\x1b[1;5A" (Ctrl+Up) and unmapped ones like "\x1b[Z"
	// (Shift+Tab) be consumed as a whole instead of leaking their trailing
	// parameter/final bytes as literal keys.
	if buf[1] == '[' {
		const maxLen = 64
		i := 2
		for i < len(buf) && i < maxLen {
			b := buf[i]
			if b >= 0x40 && b <= 0x7e {
				seq := string(buf[:i+1])
				if key, ok := fixedSequences[seq]; ok {
					return &inputEvent{kind: eventKey, key: key}, i + 1, true
				}
				// Recognized CSI shape but not a mapped key (modified
				// arrows, function keys, shift-tab, ...): discard the
				// whole sequence rather than leak its bytes.
				return nil, i + 1, true
			}
			if b < 0x20 || b == 0x7f {
				// Not a valid CSI parameter/intermediate byte (e.g. a
				// control key like Enter or Ctrl+C arrived before the
				// sequence finished). This was never a real CSI sequence,
				// so treat the ESC on its own and leave the rest of buf
				// — including this byte — to be reprocessed
				// independently, rather than swallowing a real keystroke
				// as part of a discarded sequence.
				return &inputEvent{kind: eventKey, key: KeyEsc}, 1, true
			}
			i++
		}
		if i >= maxLen {
			// Runaway/malformed sequence with no final byte after maxLen
			// body bytes: discard what's buffered so far.
			return nil, i, true
		}
		// Final byte not seen yet; keep buffering.
		return nil, 0, false
	}

	// SS3 ("\x1bO<byte>") sequence: always exactly 3 bytes, no parameter or
	// intermediate bytes.
	if buf[1] == 'O' {
		if len(buf) < 3 {
			return nil, 0, false
		}
		if key, ok := fixedSequences[string(buf[:3])]; ok {
			return &inputEvent{kind: eventKey, key: key}, 3, true
		}
		// Unrecognized SS3 final byte (e.g. F1-F4 "\x1bOP".."\x1bOS"):
		// discard the whole sequence instead of leaking its bytes.
		return nil, 3, true
	}

	// ESC followed by something that isn't '[' or 'O': treat ESC on its
	// own and let the next byte be reprocessed independently.
	return &inputEvent{kind: eventKey, key: KeyEsc}, 1, true
}

// decodeSGRMouse decodes an SGR mouse report of the form
// "\x1b[<Cb;Cx;Cy(M|m)". Returns complete=false if more bytes are needed.
// Malformed reports (too long, non-numeric fields, coordinates below 1, or
// an unrecognized button code) are discarded as a unit once complete.
func (d *inputDecoder) decodeSGRMouse() (ev *inputEvent, n int, complete bool) {
	buf := d.buf
	const maxLen = 64

	end := -1
	for i := 3; i < len(buf) && i < maxLen; i++ {
		if buf[i] == 'M' || buf[i] == 'm' {
			end = i
			break
		}
	}
	if end == -1 {
		if len(buf) >= maxLen {
			// Malformed / oversized report: discard up to maxLen bytes.
			return nil, maxLen, true
		}
		return nil, 0, false
	}

	final := buf[end]
	fields := string(buf[3:end])
	n = end + 1

	parts := strings.SplitN(fields, ";", 3)
	if len(parts) != 3 {
		return nil, n, true
	}
	cb, err1 := strconv.Atoi(parts[0])
	cx, err2 := strconv.Atoi(parts[1])
	cy, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, n, true
	}
	if cx < 1 || cy < 1 {
		return nil, n, true
	}

	release := final == 'm'

	var button mouseButton
	switch cb & 0b1100011 {
	case 0:
		button = mouseLeft
	case 1:
		button = mouseMiddle
	case 2:
		button = mouseRight
	case 64:
		button = mouseWheelUp
	case 65:
		button = mouseWheelDown
	default:
		return nil, n, true
	}

	m := mouseEvent{x: cx - 1, y: cy - 1, button: button, release: release}
	return &inputEvent{kind: eventMouse, mouse: m}, n, true
}

// Flush is called when no more input is currently available (e.g. a read
// timeout). It resolves any pending incomplete sequence that has aged past
// escapeSequenceDelay: a lone pending ESC becomes KeyEsc, while a longer
// malformed/incomplete sequence is discarded silently.
func (d *inputDecoder) Flush(now time.Time) []inputEvent {
	if len(d.buf) == 0 || d.buf[0] != 0x1b {
		return nil
	}
	if d.pendingSince.IsZero() || now.Sub(d.pendingSince) < escapeSequenceDelay {
		return nil
	}
	buf := d.buf
	d.buf = nil
	d.pendingSince = time.Time{}
	if len(buf) == 1 {
		return []inputEvent{{kind: eventKey, key: KeyEsc}}
	}
	return nil
}

// PendingTimeout reports how long the caller should wait before calling
// Flush again to resolve a pending incomplete escape sequence. ok is false
// when there is nothing pending (buf doesn't start with ESC).
func (d *inputDecoder) PendingTimeout(now time.Time) (time.Duration, bool) {
	if len(d.buf) == 0 || d.buf[0] != 0x1b {
		return 0, false
	}
	since := d.pendingSince
	if since.IsZero() {
		since = now
	}
	remaining := escapeSequenceDelay - now.Sub(since)
	if remaining < 0 {
		remaining = 0
	}
	return remaining, true
}
