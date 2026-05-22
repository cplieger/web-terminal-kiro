// Predictive local echo (INPUT-01) — Mosh-style mini-VT that advances
// a "predicted cursor" optimistically as the user types, before the
// server's next screen frame confirms or contradicts the prediction.
//
// On the wire we still send everything through the connection module;
// this is purely a local-render optimisation. The visible effect is
// that the cursor moves immediately on each keystroke even on a 200 ms
// link, so typing feels responsive instead of laggy.
//
// Scope (deliberately conservative — wrong predictions are worse than
// missing predictions, since they look like the terminal is broken):
//   - printable ASCII / UTF-8 codepoints: advance one cell per
//     codepoint (approximate for fullwidth / combining marks; snaps
//     to truth on next server frame).
//   - Backspace (0x08) and DEL (0x7f): cursor back one cell.
//   - CR (0x0d): col = 0.
//   - LF (0x0a): row += 1 (clamped to height-1).
//   - Anything else (ESC sequences, TAB, other C0): suspend prediction
//     until the next server frame. Suspending hides the predicted
//     cursor so the user sees only the server's truth.
//
// Each server screen frame resets the predicted cursor to the server's
// reported position and re-arms prediction. So a wrong prediction is
// at most one flushInterval (50 ms) old before correction.

let predRow = 0;
let predCol = 0;
let predActive = false;
let predPendingWrap = false; // matches server VT pendingWrap semantics
let cols = 80;
let rows = 30;

let onChange: (() => void) | null = null;

/** Subscribe to predicted-cursor changes (the renderer registers
 *  this so it can move its predicted-cursor overlay). */
export function subscribe(cb: () => void): void {
  onChange = cb;
}

/** Update the screen dimensions used for wrap calculations. Should be
 *  called whenever the renderer learns a new size from the server. */
export function setDimensions(c: number, r: number): void {
  if (c > 0) cols = c;
  if (r > 0) rows = r;
}

/** Reset prediction to the server's reported cursor and re-arm. */
export function onScreenFrame(serverRow: number, serverCol: number): void {
  predRow = serverRow;
  predCol = serverCol;
  predActive = true;
  predPendingWrap = false;
  if (onChange) onChange();
}

/** Apply locally-typed input bytes to the predicted cursor. Bails out
 *  (suspending prediction) on the first byte we can't model. */
export function applyInput(bytes: Uint8Array): void {
  if (!predActive) return;
  let i = 0;
  while (i < bytes.length) {
    const b = bytes[i]!;
    if (b === 0x1b) {
      // ESC — start of an escape sequence. Don't try to model it.
      predActive = false;
      if (onChange) onChange();
      return;
    }
    if (b === 0x08 || b === 0x7f) {
      // BS / DEL — backspace one cell.
      predPendingWrap = false;
      if (predCol > 0) {
        predCol--;
      } else if (predRow > 0) {
        // wrap to end of previous row (terminals typically don't,
        // but kiro-cli's prompt frequently does on Backspace at col
        // 0, so this is closer to expected behavior than staying put)
        predRow--;
        predCol = cols - 1;
      }
      i++;
      continue;
    }
    if (b === 0x0d) { // CR
      predPendingWrap = false;
      predCol = 0;
      i++;
      continue;
    }
    if (b === 0x0a) { // LF
      predPendingWrap = false;
      predRow = Math.min(predRow + 1, rows - 1);
      i++;
      continue;
    }
    if (b < 0x20) {
      // Other C0 — probably interpreted by the application; bail.
      predActive = false;
      if (onChange) onChange();
      return;
    }
    // Printable ASCII or UTF-8 lead byte. Detect the codepoint length
    // and advance one cell. Mirror the server's pendingWrap semantics
    // so the predicted cursor matches the server VT exactly: when
    // pendingWrap is set we wrap to (row+1, 0) BEFORE writing, then
    // record pendingWrap if the new col is the last one.
    let len = 1;
    if (b >= 0xc0 && b < 0xe0) len = 2;
    else if (b >= 0xe0 && b < 0xf0) len = 3;
    else if (b >= 0xf0) len = 4;
    i += len;
    if (predPendingWrap) {
      predCol = 0;
      predRow = Math.min(predRow + 1, rows - 1);
      predPendingWrap = false;
    }
    if (predCol >= cols - 1) {
      // Last column: don't advance, set pendingWrap for the next char.
      predCol = cols - 1;
      predPendingWrap = true;
    } else {
      predCol++;
    }
  }
  if (onChange) onChange();
}

/** Current predicted cursor state. `active` is false when prediction
 *  is suspended (e.g. after an unmodelled byte). When the predicted
 *  position equals the server's position, the renderer should hide
 *  the overlay so the user sees only one cursor. */
export function get(): { row: number; col: number; active: boolean } {
  return { row: predRow, col: predCol, active: predActive };
}
