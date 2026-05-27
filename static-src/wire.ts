// Pure wire-format helpers for the vibecli WebSocket protocol.
//
// Protocol: every WebSocket message vibecli sends to the server is a
// binary frame whose first byte tags the message type:
//
//   0x00  control message (JSON-encoded {type, ...} payload)
//   any   raw terminal input bytes
//
// The 0x00 prefix is the only framing convention; raw input flows
// through the rest of the byte space (xterm.js writes UTF-8 + ANSI
// escapes, none of which collide with the single-byte 0x00 tag at
// frame start because terminal input never starts a write with NUL).
// See terminal.go's frame parser for the receiving end.
//
// Pulled into a dedicated module so unit tests can exercise the
// framing without spinning up a WebSocket. Pure: no DOM, no
// WebSocket, no module-level state.

/** Type tag prefix for control messages. */
export const CONTROL_FRAME_PREFIX = 0x00;

/**
 * controlFrame builds the 0x00-prefixed binary frame containing the
 * JSON encoding of msg. The result is the exact bytes to pass to
 * `WebSocket.send(...)`.
 *
 * Generic over the message type so callers retain compile-time
 * checking of the JSON shape. Returns a Uint8Array backed by a
 * fresh ArrayBuffer (not a SharedArrayBuffer slice) so it satisfies
 * WebSocket.send's BufferSource parameter.
 */
export function controlFrame(msg: unknown): Uint8Array<ArrayBuffer> {
  const body = new TextEncoder().encode(JSON.stringify(msg));
  const frame = new Uint8Array(new ArrayBuffer(body.length + 1));
  frame[0] = CONTROL_FRAME_PREFIX;
  frame.set(body, 1);
  return frame;
}
