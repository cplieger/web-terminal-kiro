package terminal

import (
	"github.com/coder/websocket"

	"vibecli/internal/vt"
)

// FlushFrameBuilder computes outbound flush frames by diffing the
// current screen state against the previously sent state. It owns the
// prev-row comparison data so buildFrame can be expressed as a method
// on this type rather than reaching into Handler fields.
type FlushFrameBuilder struct {
	prevRowWires   [][]vt.WireRun
	prevCurRow     int
	prevCurCol     int
	prevBracketed  bool
	prevAppCursor  bool
	modesAnnounced bool
	prevCurValid   bool
}

// Reset clears the previous-row cache, forcing the next frame to
// treat all rows as changed (used after resize or new client connect).
func (b *FlushFrameBuilder) Reset() {
	b.prevRowWires = nil
	b.prevCurValid = false
}

// Build computes the next outbound frame from the current screen state
// and client snapshot. Returns nil if there is nothing to send.
func (b *FlushFrameBuilder) Build(screen *vt.Screen, resized bool, clients map[*websocket.Conn]uint64) *flushFrame {
	if !resized {
		screen.DrainScrollback()
		return nil
	}
	if screen.IsFlushHeld() {
		return nil
	}

	drained := screen.DrainScrollback()
	var scrollOut [][]vt.WireRun
	if !screen.InAltScreen && len(drained) > 0 {
		scrollOut = drained
	}

	rows := make([][]vt.WireRun, screen.Height)
	for y := range screen.Height {
		rows[y] = screen.RenderRowWire(y)
	}
	curRow, curCol := screen.CursorPos()

	bell := screen.BellRing
	screen.BellRing = false

	var changed []int
	for y, row := range rows {
		if y >= len(b.prevRowWires) || !runsEqual(b.prevRowWires[y], row) {
			changed = append(changed, y)
		}
	}
	b.prevRowWires = rows

	// Cursor-only moves (e.g. typing a space onto an existing space cell,
	// or left/right arrow which only emit cursor-position CSI without
	// changing any cell content) leave `changed` empty but still need a
	// frame so the client can repaint the cursor at its new position.
	cursorMoved := !b.prevCurValid || curRow != b.prevCurRow || curCol != b.prevCurCol
	b.prevCurRow = curRow
	b.prevCurCol = curCol
	b.prevCurValid = true

	if len(changed) == 0 && len(scrollOut) == 0 && b.modesStable(screen) && !cursorMoved {
		return nil
	}

	var modesPayload []byte
	curBracketed := screen.BracketedPaste
	curAppCursor := screen.AppCursorKeys
	if !b.modesAnnounced || curBracketed != b.prevBracketed || curAppCursor != b.prevAppCursor {
		modesPayload = encodeModesMsg(0, curBracketed, curAppCursor)
		b.prevBracketed = curBracketed
		b.prevAppCursor = curAppCursor
		b.modesAnnounced = true
	}

	return &flushFrame{
		clients:      clients,
		rows:         rows,
		scrollLines:  scrollOut,
		changed:      changed,
		curRow:       curRow,
		curCol:       curCol,
		screenHeight: screen.Height,
		cursorStyle:  screen.CursorStyle,
		cursorHidden: screen.CursorHidden,
		cursorBlink:  screen.CursorBlink,
		modesPayload: modesPayload,
		bell:         bell,
	}
}

// modesStable reports whether the screen's DEC private mode state
// matches the last announced values.
func (b *FlushFrameBuilder) modesStable(screen *vt.Screen) bool {
	return b.modesAnnounced &&
		screen.BracketedPaste == b.prevBracketed &&
		screen.AppCursorKeys == b.prevAppCursor
}
