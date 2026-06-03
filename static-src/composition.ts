// IME / composition support.
//
// Mirrors xterm.js's CompositionHelper. The browser fires
// compositionstart → compositionupdate(s) → compositionend when an IME
// (Japanese / Korean / Chinese / Vietnamese / dictation / autocorrect
// suggestion bar) is active. Without a handler, the user sees nothing
// while typing because keydown/input fire only when the composed
// phrase is finalised.
//
// What this module does:
//   1. On compositionstart, mark composing=true and capture textarea
//      selection bounds. Show the floating compositionView at the
//      cursor with the in-progress text.
//   2. On compositionupdate, mirror the in-progress text into
//      compositionView (\u200E LTR markers, RTL CSS direction so long
//      compositions track their tail).
//   3. On compositionend, set a setTimeout(0) finaliser. The
//      compositionend.data property is unreliable on Chromium (Korean
//      ending consonant moves to next char on vowel input, etc.); the
//      textarea value at next tick has the truth.
//   4. While composing, callers' keydown handler must early-return
//      (handled via isComposing()). xterm.js takes the same approach.
//
// Positioning: the compositionView is absolutely positioned inside
// `termWrap`. The IME UI (browser-rendered popup with candidate words)
// uses the textarea as its anchor; we move the textarea to the cursor
// for the same reason.

import { keyboard } from "@cplieger/vterm";
const { bracketTextForPaste, prepareTextForTerminal } = keyboard;

// Single-character invisible placeholder used by app.ts to keep the
// iOS soft keyboard's held-Backspace auto-repeat firing. Composition
// finalisation must restore the placeholder (and not empty the
// textarea) for the same reason.
const INPUT_PLACEHOLDER = "\u00A0";

let textarea: HTMLTextAreaElement;
let compositionView: HTMLElement;
let getCursorPx: () => { left: number; top: number; cellH: number };
let send: (bytes: string) => void;

let composing = false;
let sendingComposition = false;
let compositionStart = 0;
let compositionEnd = 0;
let compositionSuffix = "";

export function init(opts: {
  textarea: HTMLTextAreaElement;
  compositionView: HTMLElement;
  getCursorPx: () => { left: number; top: number; cellH: number };
  send: (bytes: string) => void;
}): void {
  textarea = opts.textarea;
  compositionView = opts.compositionView;
  getCursorPx = opts.getCursorPx;
  send = opts.send;

  textarea.addEventListener("compositionstart", onStart);
  textarea.addEventListener("compositionupdate", onUpdate);
  textarea.addEventListener("compositionend", onEnd);
  textarea.addEventListener("paste", onPaste);
}

/** True while an IME composition is in progress. Caller's keydown
 *  listener must early-return on true to avoid sending raw key bytes
 *  during composition. */
export function isComposing(): boolean {
  return composing || sendingComposition;
}

function onStart(): void {
  composing = true;
  const start = textarea.selectionStart;
  const end = textarea.selectionEnd;
  compositionStart = Math.min(start, end);
  compositionEnd = Math.max(start, end);
  compositionSuffix = textarea.value.substring(compositionEnd);
  compositionView.textContent = "";
  compositionView.classList.add("active");
  positionCompositionView();
}

function onUpdate(ev: CompositionEvent): void {
  // \u200E (LTR mark) wrappers + CSS direction:rtl on the view make
  // long compositions show their trailing edge instead of being
  // clipped at the start. Pattern from xterm.js.
  compositionView.textContent = `\u200E${ev.data}\u200E`;
  positionCompositionView();
  // Schedule a microtask to refresh the end position after the
  // textarea selection settles.
  setTimeout(() => {
    const end = textarea.selectionEnd;
    compositionEnd = Math.max(compositionStart, end);
  }, 0);
}

function onEnd(): void {
  compositionView.classList.remove("active");
  composing = false;
  // The compositionend event fires before the textarea reflects the
  // final value on most browsers (Chromium especially). Defer one tick
  // and read the textarea then; xterm.js's pattern.
  sendingComposition = true;
  const startSnapshot = compositionStart;
  const suffixSnapshot = compositionSuffix;
  setTimeout(() => {
    if (!sendingComposition) {
      return;
    }
    sendingComposition = false;
    const value = textarea.value;
    const valueEnd =
      suffixSnapshot.length > 0 && value.endsWith(suffixSnapshot)
        ? value.length - suffixSnapshot.length
        : value.length;
    const composed = value.substring(startSnapshot, Math.max(startSnapshot, valueEnd));
    if (composed.length > 0) {
      send(composed);
    }
    resetTextareaToPlaceholder();
  }, 0);
}

function onPaste(ev: ClipboardEvent): void {
  // Native `paste` event handler. Required for iOS where Ctrl+Shift+V
  // is unavailable; users invoke paste from the iOS callout menu and
  // it fires this event on the focused textarea.
  if (!ev.clipboardData) {
    return;
  }
  const raw = ev.clipboardData.getData("text/plain");
  if (raw === "") {
    return;
  }
  ev.preventDefault();
  ev.stopPropagation();
  send(bracketTextForPaste(prepareTextForTerminal(raw)));
  // Restore the placeholder so the subsequent `input` event (some
  // browsers fire it after paste) and held-Backspace auto-repeat keep
  // working.
  resetTextareaToPlaceholder();
}

function resetTextareaToPlaceholder(): void {
  textarea.value = INPUT_PLACEHOLDER;
  try {
    textarea.setSelectionRange(INPUT_PLACEHOLDER.length, INPUT_PLACEHOLDER.length);
  } catch {
    // Some older WebKit builds throw on setSelectionRange against a
    // visually-hidden textarea; ignore.
  }
}

/** Position the composition view (and the helper textarea, which
 *  anchors the IME popup) at the current terminal cursor pixel
 *  position. iOS keyboards open without scrolling the layout when
 *  the focused textarea is on screen at a sane location. */
export function positionCompositionView(): void {
  const { left, top, cellH } = getCursorPx();
  compositionView.style.left = `${left}px`;
  compositionView.style.top = `${top}px`;
  compositionView.style.height = `${cellH}px`;
  compositionView.style.lineHeight = `${cellH}px`;
  // Textarea piggy-backs on the cursor position so iOS scroll-into-
  // view targets the cursor area instead of jumping to top-left.
  textarea.style.left = `${left}px`;
  textarea.style.top = `${top}px`;
  textarea.style.height = `${cellH}px`;
}
