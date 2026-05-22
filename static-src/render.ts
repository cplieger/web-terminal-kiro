// Render layer: single-container model.
//
// One flat list of term-row divs inside #term-output. The last N rows
// are the "live zone" (overwritten by screen frames). Everything above
// is frozen history — either from scroll messages (lines that fell off
// the server's VT screen) or from snapshots of live content before it
// was overwritten. Users scroll up through this history naturally.
//
// Cap: MAX_HISTORY rows of frozen history. Oldest get evicted.

import type { ScreenMessage, ScrollMessage, WireRun } from "./types.js";
import * as scroll from "./scroll.js";

// --- Width cache (two-tier, xterm.js style) ---
const WIDTH_FLAT_SIZE = 256;
const WIDTH_FLAT_UNSET = -9999;
const widthFlat = new Float32Array(WIDTH_FLAT_SIZE).fill(WIDTH_FLAT_UNSET);
const widthMap = new Map<string, number>();

const VARIANT_REGULAR = 0;
const VARIANT_BOLD = 1;
const VARIANT_ITALIC = 2;
const variantCtx: (CanvasRenderingContext2D | null)[] = [null, null, null, null];
let fontString = "";

function variantContext(variant: number): CanvasRenderingContext2D {
  let ctx = variantCtx[variant];
  if (ctx) return ctx;
  const canvas = document.createElement("canvas");
  canvas.width = 1;
  canvas.height = 1;
  ctx = canvas.getContext("2d")!;
  let f = "";
  if (variant & VARIANT_ITALIC) f += "italic ";
  if (variant & VARIANT_BOLD) f += "bold ";
  f += fontString;
  ctx.font = f;
  variantCtx[variant] = ctx;
  return ctx;
}

function resetVariantContexts(): void {
  for (let i = 0; i < variantCtx.length; i++) variantCtx[i] = null;
}

function measureChar(ch: string, bold: boolean, italic: boolean): number {
  if (!bold && !italic && ch.length === 1) {
    const cp = ch.charCodeAt(0);
    if (cp < WIDTH_FLAT_SIZE) {
      const cached = widthFlat[cp]!;
      if (cached !== WIDTH_FLAT_UNSET) return cached;
      const w = variantContext(VARIANT_REGULAR).measureText(ch).width;
      widthFlat[cp] = w;
      return w;
    }
  }
  const key = (bold ? "B" : "") + (italic ? "I" : "") + ch;
  const cached = widthMap.get(key);
  if (cached !== undefined) return cached;
  let variant = 0;
  if (bold) variant |= VARIANT_BOLD;
  if (italic) variant |= VARIANT_ITALIC;
  const w = variantContext(variant).measureText(ch).width;
  widthMap.set(key, w);
  return w;
}

function measureCellWidth(): number {
  // Measure using a span appended to termWrap (which already has the
  // font applied via CSS). This ensures the web font is used if loaded.
  const span = document.createElement("span");
  span.style.visibility = "hidden";
  span.style.position = "absolute";
  span.style.whiteSpace = "pre";
  span.textContent = "MMMMMMMMMM";
  termWrap.appendChild(span);
  const width = span.getBoundingClientRect().width / 10;
  termWrap.removeChild(span);
  return width;
}

// --- State ---
let output: HTMLElement;
let termWrap: HTMLElement;

const MAX_HISTORY = 1000;
let allRows: HTMLDivElement[] = [];
let liveCount = 0;
let cursorRow = 0;
let cursorCol = 0;
let cursorHidden = false;
let cursorStyleVal = 0; // 0-6: DECSCUSR

function cursorClassName(): string {
  // DECSCUSR: 0/1=blinking block, 2=steady block, 3=blinking underline,
  // 4=steady underline, 5=blinking bar, 6=steady bar
  if (cursorStyleVal === 3 || cursorStyleVal === 4) return "term-cursor-underline";
  if (cursorStyleVal === 5 || cursorStyleVal === 6) return "term-cursor-bar";
  return "term-cursor";
}
let cellWidth = 8;
let cellHeight = 17;
let defaultSpacing = 0;
let firstScreen = true;
let onCursorMove: (() => void) | null = null;

export function init(opts: {
  output: HTMLElement;
  termWrap: HTMLElement;
  onCursorMove?: () => void;
}): void {
  output = opts.output;
  termWrap = opts.termWrap;
  onCursorMove = opts.onCursorMove ?? null;
  startCursorBlink();
}

export function resetScreen(): void {
  firstScreen = true;
}

export function resetScrollback(): void {
  const historyCount = allRows.length - liveCount;
  for (let i = 0; i < historyCount; i++) {
    allRows[i]!.remove();
  }
  allRows = allRows.slice(historyCount);
}

// --- Color helpers ---
function colorHex(c: number | undefined): string | null {
  if (c === undefined || c < 0) return null;
  return "#" + c.toString(16).padStart(6, "0");
}

// --- URL detection (xterm.js addon-web-links pattern) ---
const URL_RE = /(https?|HTTPS?):\/\/[^\s"'!*(){}|\\^<>`]*[^\s"':,.!?{}|\\^~[\]`()<>]/g;

function linkifySpans(spans: (HTMLSpanElement | HTMLAnchorElement)[]): (HTMLSpanElement | HTMLAnchorElement)[] {
  const out: (HTMLSpanElement | HTMLAnchorElement)[] = [];
  for (const span of spans) {
    const text = span.textContent ?? "";
    URL_RE.lastIndex = 0;
    let match: RegExpExecArray | null;
    let last = 0;
    let found = false;
    while ((match = URL_RE.exec(text)) !== null) {
      found = true;
      if (match.index > last) {
        const pre = span.cloneNode(false) as HTMLSpanElement;
        pre.textContent = text.slice(last, match.index);
        out.push(pre);
      }
      const a = document.createElement("a");
      a.href = match[0];
      a.target = "_blank";
      a.rel = "noopener noreferrer";
      a.className = "term-link";
      a.textContent = match[0];
      // Copy inline styles from the source span
      a.style.cssText = span.style.cssText;
      out.push(a);
      last = match.index + match[0].length;
    }
    if (!found) {
      out.push(span);
    } else if (last < text.length) {
      const post = span.cloneNode(false) as HTMLSpanElement;
      post.textContent = text.slice(last);
      out.push(post);
    }
  }
  return out;
}

// --- Build row DOM ---
function buildRowSpans(runs: WireRun[], cursorAt: number): (HTMLSpanElement | HTMLAnchorElement)[] {
  const out: HTMLSpanElement[] = [];
  let col = 0;
  for (const run of runs) {
    if (!run.t) continue;
    const attrs = run.a || 0;
    const isBold = (attrs & 1) !== 0;
    const isItalic = (attrs & 2) !== 0;
    const isUnderline = (attrs & 4) !== 0;
    const isInverse = (attrs & 8) !== 0;
    const isStrike = (attrs & 16) !== 0;
    const isDim = (attrs & 32) !== 0;
    const isHidden = (attrs & 64) !== 0;
    const isBlink = (attrs & 128) !== 0;
    const isOverline = (attrs & 256) !== 0;
    const isDoubleUnderline = (attrs & 512) !== 0;

    // Server swaps FG/BG for inverse in wire.go, but when both are
    // default (-1) the swap is a no-op. Detect inverse + defaults and
    // apply theme-inverted colors so the inverted space is visible.
    let fg = colorHex(run.f);
    let bg = colorHex(run.b);
    if (isInverse && fg === null && bg === null) {
      fg = "var(--bg)";
      bg = "var(--text)";
    }
    const ucColor = colorHex(run.uc);

    const applyStyle = (span: HTMLSpanElement, spacing: number): void => {
      if (isHidden) { span.style.visibility = "hidden"; }
      if (fg !== null) span.style.color = fg;
      if (bg !== null) span.style.background = bg;
      if (isBold) span.style.fontWeight = "bold";
      if (isItalic) span.style.fontStyle = "italic";
      if (isDim) span.style.opacity = ".5";
      // Build text-decoration combining all line types.
      const decoLines: string[] = [];
      if (isDoubleUnderline) decoLines.push("underline");
      else if (isUnderline) decoLines.push("underline");
      if (isOverline) decoLines.push("overline");
      if (isStrike) decoLines.push("line-through");
      if (decoLines.length > 0) {
        let deco = decoLines.join(" ");
        if (isDoubleUnderline) deco += " double";
        span.style.textDecoration = deco;
      }
      if (ucColor !== null) span.style.textDecorationColor = ucColor;
      if (spacing !== defaultSpacing) span.style.letterSpacing = `${spacing}px`;
      if (isBlink) span.classList.add("term-blink");
    };

    let prevSpacing: number | null = null;
    let buffer = "";
    const flush = (): void => {
      if (buffer.length === 0) return;
      const span = document.createElement("span");
      span.textContent = buffer;
      applyStyle(span, prevSpacing ?? 0);
      out.push(span);
      buffer = "";
    };
    for (const ch of run.t) {
      if (ch === "\uFFFF") {
        // Wide-char continuation placeholder: mark previous span as double-width.
        if (out.length > 0) {
          const prev = out[out.length - 1]!;
          const prevText = prev.textContent ?? "";
          if (prevText.length > 0) {
            const lastChar = [...prevText].at(-1)!;
            const w = measureChar(lastChar, isBold, isItalic);
            prev.style.letterSpacing = `${cellWidth * 2 - w}px`;
          }
        }
        continue;
      }
      if (col === cursorAt) {
        flush();
        const w = measureChar(ch, isBold, isItalic);
        const spacing = cellWidth - w;
        const span = document.createElement("span");
        span.className = cursorClassName();
        span.textContent = ch;
        if (spacing !== defaultSpacing) span.style.letterSpacing = `${spacing}px`;
        out.push(span);
        col++;
        continue;
      }
      const w = measureChar(ch, isBold, isItalic);
      const spacing = cellWidth - w;
      if (prevSpacing === null) {
        prevSpacing = spacing;
      } else if (spacing !== prevSpacing) {
        flush();
        prevSpacing = spacing;
      }
      buffer += ch;
      col++;
    }
    flush();
  }
  if (cursorAt >= 0 && col <= cursorAt) {
    while (col < cursorAt) {
      const span = document.createElement("span");
      span.textContent = " ";
      out.push(span);
      col++;
    }
    const cursor = document.createElement("span");
    cursor.className = cursorClassName();
    cursor.textContent = " ";
    out.push(cursor);
  }
  if (out.length === 0) {
    const span = document.createElement("span");
    span.innerHTML = "&nbsp;";
    out.push(span);
  }
  return linkifySpans(out);
}

// --- Live zone management ---
function ensureLiveZone(count: number): void {
  while (liveCount < count) {
    const div = document.createElement("div");
    div.className = "term-row";
    output.appendChild(div);
    allRows.push(div);
    liveCount++;
  }
  while (liveCount > count) {
    const el = allRows.pop();
    if (el) el.remove();
    liveCount--;
  }
}

function trimHistory(): void {
  const historyCount = allRows.length - liveCount;
  if (historyCount > MAX_HISTORY) {
    const excess = historyCount - MAX_HISTORY;
    for (let i = 0; i < excess; i++) {
      allRows[i]!.remove();
    }
    allRows = allRows.slice(excess);
  }
}

// --- Screen frame handling ---
let pendingRows: WireRun[][] | null = null;
let pendingCursor: [number, number] | null = null;
let pendingChanged: Set<number> = new Set();
let pendingCursorHidden = false;
let pendingCursorStyle = 0;
let pendingCursorBlink = true;
let pendingBell = false;
let pendingFrame: number | undefined;

export function handleScreen(msg: ScreenMessage): void {
  pendingRows = msg.rows;
  pendingCursor = msg.cursor;
  pendingCursorHidden = msg.cursorHidden ?? false;
  pendingCursorStyle = msg.cursorStyle ?? 0;
  pendingCursorBlink = msg.cursorBlink ?? true;
  if (msg.bell) pendingBell = true;
  for (const idx of msg.changed) pendingChanged.add(idx);
  if (pendingFrame !== undefined) return;
  pendingFrame = requestAnimationFrame(flushScreen);
}

function flushScreen(): void {
  pendingFrame = undefined;
  if (pendingRows === null || pendingCursor === null) return;
  const rows = pendingRows;
  const cursor = pendingCursor;
  const changed = pendingChanged;
  const hidden = pendingCursorHidden;
  const style = pendingCursorStyle;
  const blink = pendingCursorBlink;
  const bell = pendingBell;
  pendingRows = null;
  pendingCursor = null;
  pendingChanged = new Set();
  pendingBell = false;

  try {
    flushScreenInner(rows, cursor, changed, hidden, style, bell, blink);
  } catch (err) {
    console.error("vibecli: render error", err);
  }
}

function flushScreenInner(rows: WireRun[][], cursor: [number, number], changed: Set<number>, msg_cursorHidden: boolean, msg_cursorStyle: number, bell: boolean, msg_cursorBlink: boolean): void {
  if (firstScreen) {
    output.innerHTML = "";
    allRows = [];
    liveCount = 0;
    firstScreen = false;
  }
  ensureLiveZone(rows.length);

  const newCursorRow = cursor[0];
  const newCursorCol = cursor[1];

  if (newCursorRow !== cursorRow) {
    changed.add(cursorRow);
    changed.add(newCursorRow);
  } else if (newCursorCol !== cursorCol) {
    changed.add(newCursorRow);
  }

  cursorRow = newCursorRow;
  cursorCol = newCursorCol;
  cursorHidden = msg_cursorHidden;
  cursorStyleVal = msg_cursorStyle;
  setCursorBlink(msg_cursorBlink);

  const liveStart = allRows.length - liveCount;
  for (const idx of changed) {
    const row = rows[idx];
    const el = allRows[liveStart + idx];
    if (row !== undefined && el) {
      const cursorAt = (!cursorHidden && idx === cursorRow) ? cursorCol : -1;
      const newSpans = buildRowSpans(row, cursorAt);
      // Skip DOM update if the text content is identical AND the cursor
      // is not involved — preserves browser find-in-page highlights and
      // avoids unnecessary reflows. Cursor rows always update because
      // the cursor span class/position may differ even when text is the
      // same (arrow keys, space-over-space, Enter clearing input).
      const hasCursor = cursorAt >= 0;
      const hadCursor = el.querySelector(".term-cursor, .term-cursor-underline, .term-cursor-bar") !== null;
      if (!hasCursor && !hadCursor) {
        const newText = newSpans.map(s => s.textContent).join("");
        if (el.textContent === newText && el.childElementCount > 0) {
          continue;
        }
      }
      el.replaceChildren(...newSpans);
    }
  }
  if (onCursorMove) onCursorMove();

  // Collapse the browser's internal caret to the end of the
  // contenteditable so typing doesn't scroll to the top — but only
  // when the user has no active text selection (preserves copy ability
  // during spinner/progress updates).
  const sel = window.getSelection();
  if (sel && document.activeElement === output && sel.isCollapsed) {
    sel.selectAllChildren(output);
    sel.collapseToEnd();
  }

  if (bell) {
    output.classList.add("term-bell");
    setTimeout(() => output.classList.remove("term-bell"), 150);
  }

  if (!scroll.isUserScrolledUp() && !scroll.isInUserScroll() && pendingScrollback.length === 0) {
    scroll.scrollToBottom();
  }
}

// --- Scroll message handling (lines that fell off the server's screen) ---
// These get inserted as frozen history above the live zone.
const pendingScrollback: WireRun[][] = [];
let pendingScrollFrame = false;

export function handleScroll(msg: ScrollMessage): void {
  if (msg.lines.length === 0) return;
  pendingScrollback.push(...msg.lines);
  if (!pendingScrollFrame) {
    pendingScrollFrame = true;
    requestAnimationFrame(processPendingScrollback);
  }
}

function processPendingScrollback(): void {
  pendingScrollFrame = false;
  // Process ALL pending lines in one shot — no per-frame cap.
  const batch = pendingScrollback.splice(0);

  const liveStart = allRows.length - liveCount;
  const refNode = liveStart < allRows.length ? allRows[liveStart]! : null;

  for (const line of batch) {
    const div = document.createElement("div");
    div.className = "term-row";
    div.replaceChildren(...buildRowSpans(line, -1));
    output.insertBefore(div, refNode);
    allRows.splice(allRows.length - liveCount, 0, div);
  }

  trimHistory();

  if (!scroll.isInUserScroll()) {
    scroll.scrollToBottom();
  }
}

// --- Cursor blink ---
const CURSOR_BLINK_MS = 530;
let blinkInterval: ReturnType<typeof setInterval> | null = null;
let blinkEnabled = true;

function startCursorBlink(): void {
  if (blinkInterval !== null) return;
  output.classList.remove("cursor-blink-off");
  blinkInterval = setInterval(() => {
    output.classList.toggle("cursor-blink-off");
  }, CURSOR_BLINK_MS);
}

function stopCursorBlink(): void {
  if (blinkInterval !== null) {
    clearInterval(blinkInterval);
    blinkInterval = null;
  }
  output.classList.remove("cursor-blink-off");
}

/** Called from flushScreenInner when cursorBlink state changes. */
function setCursorBlink(enabled: boolean): void {
  if (enabled === blinkEnabled) return;
  blinkEnabled = enabled;
  if (enabled) startCursorBlink();
  else stopCursorBlink();
}

// --- Font metrics & sizing ---
export function updateFontMetrics(): void {
  const cs = window.getComputedStyle(termWrap);
  const fontSize = cs.fontSize;
  const family = cs.fontFamily;
  fontString = `${fontSize} ${family}`;
  widthFlat.fill(WIDTH_FLAT_UNSET);
  widthMap.clear();
  resetVariantContexts();
  const measuredW = measureCellWidth();
  cellWidth = Math.round(measuredW);
  cellHeight = parseFloat(cs.lineHeight) || 17;
  defaultSpacing = cellWidth - measuredW;
  output.style.letterSpacing = `${defaultSpacing}px`;
  document.documentElement.style.setProperty("--char-w", `${cellWidth}px`);
}

const MIN_COLS = 20;
const MIN_ROWS = 5;

export function computeSize(): { cols: number; rows: number } {
  const cs = window.getComputedStyle(termWrap);
  const padX = parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight);
  const padY = parseFloat(cs.paddingTop) + parseFloat(cs.paddingBottom);
  const contentW = termWrap.clientWidth - padX;
  const contentH = termWrap.clientHeight - padY;
  const cols = Math.max(MIN_COLS, Math.floor(contentW / cellWidth));
  const rows = Math.max(MIN_ROWS, Math.floor(contentH / cellHeight));
  return { cols, rows };
}

export function getCursorPx(): { left: number; top: number; cellH: number } {
  const cs = window.getComputedStyle(termWrap);
  const padL = parseFloat(cs.paddingLeft);
  const padT = parseFloat(cs.paddingTop);
  // Cursor position relative to the output container's top, offset by
  // the history height above the live zone.
  const liveStart = allRows.length - liveCount;
  const historyHeight = liveStart > 0 ? (allRows[liveStart]?.offsetTop ?? 0) : 0;
  return {
    left: Math.round(padL + cursorCol * cellWidth),
    top: Math.round(padT + historyHeight + cursorRow * cellHeight),
    cellH: cellHeight,
  };
}

export function setPredictedCursor(row: number, col: number, active: boolean): void {
  const el = predCursorEl ?? (predCursorEl = document.getElementById("pred-cursor"));
  if (!el) return;
  if (!active || (row === cursorRow && col === cursorCol)) {
    el.classList.remove("visible");
    return;
  }
  const cs = window.getComputedStyle(termWrap);
  const padL = parseFloat(cs.paddingLeft);
  const padT = parseFloat(cs.paddingTop);
  const liveStart = allRows.length - liveCount;
  const historyHeight = liveStart > 0 ? (allRows[liveStart]?.offsetTop ?? 0) : 0;
  el.style.left = `${Math.round(padL + col * cellWidth)}px`;
  el.style.top = `${Math.round(padT + historyHeight + row * cellHeight)}px`;
  el.style.width = `${cellWidth}px`;
  el.style.height = `${cellHeight}px`;
  el.classList.add("visible");
}

let predCursorEl: HTMLElement | null = null;
