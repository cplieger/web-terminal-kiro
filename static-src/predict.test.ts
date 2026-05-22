// Unit tests for predict.ts (INPUT-01).
//
// Covers the predictive-cursor mini-VT under each modelled byte and
// each bail-out condition. The bail rules are the safety property:
// wrong predictions are worse than missing predictions, so anything
// we don't model must suspend, not guess.

import { describe, it, expect, beforeEach } from "vitest";
import * as predict from "./predict.js";

const enc = (s: string): Uint8Array => new TextEncoder().encode(s);

beforeEach(() => {
  predict.setDimensions(80, 24);
  predict.onScreenFrame(0, 0);
});

describe("predict: printable advances the column", () => {
  it("ASCII", () => {
    predict.applyInput(enc("abc"));
    expect(predict.get()).toEqual({ row: 0, col: 3, active: true });
  });
  it("multi-byte UTF-8 codepoint = one cell", () => {
    predict.applyInput(enc("héllo")); // é is 2 bytes
    expect(predict.get()).toEqual({ row: 0, col: 5, active: true });
  });
  it("4-byte UTF-8 (emoji) = one cell (approximate)", () => {
    predict.applyInput(enc("🙂x")); // 🙂 is 4 bytes
    expect(predict.get()).toEqual({ row: 0, col: 2, active: true });
  });
});

describe("predict: backspace and DEL", () => {
  it("BS / DEL go back one column", () => {
    predict.onScreenFrame(0, 5);
    predict.applyInput(new Uint8Array([0x7f]));
    expect(predict.get()).toEqual({ row: 0, col: 4, active: true });
    predict.applyInput(new Uint8Array([0x08]));
    expect(predict.get()).toEqual({ row: 0, col: 3, active: true });
  });

  it("at col 0, BS wraps to end of previous row", () => {
    predict.onScreenFrame(2, 0);
    predict.applyInput(new Uint8Array([0x7f]));
    expect(predict.get()).toEqual({ row: 1, col: 79, active: true });
  });

  it("at row 0 col 0, BS does nothing", () => {
    predict.applyInput(new Uint8Array([0x7f]));
    expect(predict.get()).toEqual({ row: 0, col: 0, active: true });
  });
});

describe("predict: CR / LF", () => {
  it("CR resets column", () => {
    predict.onScreenFrame(2, 30);
    predict.applyInput(new Uint8Array([0x0d]));
    expect(predict.get()).toEqual({ row: 2, col: 0, active: true });
  });
  it("LF advances row, clamped at height-1", () => {
    predict.onScreenFrame(22, 10);
    predict.applyInput(new Uint8Array([0x0a]));
    expect(predict.get()).toEqual({ row: 23, col: 10, active: true });
    predict.applyInput(new Uint8Array([0x0a])); // already at height-1
    expect(predict.get()).toEqual({ row: 23, col: 10, active: true });
  });
});

describe("predict: line wrap on printable", () => {
  it("uses pendingWrap: at last col, first char stays, second wraps", () => {
    predict.setDimensions(5, 10);
    predict.onScreenFrame(0, 4);
    predict.applyInput(enc("x"));
    // First 'x' written at (0,4); cursor stays with pendingWrap.
    expect(predict.get()).toEqual({ row: 0, col: 4, active: true });
    predict.applyInput(enc("y"));
    // Second char triggers wrap to (1,0), then advances to (1,1).
    expect(predict.get()).toEqual({ row: 1, col: 1, active: true });
  });

  it("at last row, wrap clamps row at height-1", () => {
    predict.setDimensions(5, 3);
    predict.onScreenFrame(2, 4);
    predict.applyInput(enc("xy"));
    // Second char wraps but row stays at 2 (already at height-1),
    // cursor lands at (2,1).
    expect(predict.get()).toEqual({ row: 2, col: 1, active: true });
  });
});

describe("predict: bails on unmodelled bytes", () => {
  it("ESC suspends prediction", () => {
    predict.onScreenFrame(0, 5);
    predict.applyInput(enc("a")); // ok
    predict.applyInput(new Uint8Array([0x1b])); // bail
    expect(predict.get().active).toBe(false);
  });

  it("TAB suspends prediction", () => {
    predict.applyInput(new Uint8Array([0x09]));
    expect(predict.get().active).toBe(false);
  });

  it("after bail, subsequent input is ignored until next screen frame", () => {
    predict.applyInput(new Uint8Array([0x1b]));
    predict.applyInput(enc("aaa"));
    expect(predict.get().col).toBe(0); // unchanged after bail
    // ScreenFrame re-arms.
    predict.onScreenFrame(0, 5);
    predict.applyInput(enc("a"));
    expect(predict.get()).toEqual({ row: 0, col: 6, active: true });
  });
});

describe("predict: setDimensions invalid input ignored", () => {
  it("zero or negative cols/rows leaves previous values", () => {
    predict.setDimensions(40, 12);
    predict.setDimensions(0, 0);
    predict.onScreenFrame(0, 39);
    predict.applyInput(enc("xy"));
    // Width still 40: first char stays at (0,39) with pendingWrap,
    // second char wraps to (1,0) then advances to (1,1).
    expect(predict.get()).toEqual({ row: 1, col: 1, active: true });
  });
});

describe("predict: setDimensions clamps position", () => {
  it("shrinking cols clamps predCol", () => {
    predict.setDimensions(80, 24);
    predict.onScreenFrame(0, 70);
    predict.applyInput(enc("a")); // col = 71
    predict.setDimensions(50, 24); // clamp to 49
    expect(predict.get().col).toBe(49);
  });

  it("shrinking rows clamps predRow", () => {
    predict.setDimensions(80, 24);
    predict.onScreenFrame(20, 0);
    predict.setDimensions(80, 10); // clamp to 9
    expect(predict.get().row).toBe(9);
  });
});

describe("predict: reset()", () => {
  it("clears all state back to origin", () => {
    predict.onScreenFrame(5, 10);
    predict.applyInput(enc("abc"));
    predict.reset();
    expect(predict.get()).toEqual({ row: 0, col: 0, active: false });
  });
});
