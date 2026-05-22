// vibecli client entry point: acquires DOM refs, initializes the render
// and connection layers, and wires up input + UI controls.

import * as render from "./render.js";
import * as connection from "./connection.js";
import * as scroll from "./scroll.js";
import * as viewport from "./viewport.js";
import * as composition from "./composition.js";
import * as status from "./status.js";
import * as predict from "./predict.js";
import { mapKeyboardEvent, bracketTextForPaste, prepareTextForTerminal } from "./keyboard.js";

// --- DOM refs ---
const outputEl = document.getElementById("term-output")!;
const termWrap = document.getElementById("term")!;
const input = document.getElementById("term-input") as HTMLTextAreaElement;
const compositionViewEl = document.getElementById("composition-view")!;

const encoder = new TextEncoder();
function send(bytes: string): void {
  // Suppress backspace (DEL) when the predicted cursor is at column 0
  // — there's nothing left to delete. This mimics the natural brake
  // that iOS's textarea provided (stops key-repeat when empty).
  if (bytes === "\x7f" && predict.get().col === 0 && predict.get().active) {
    return;
  }
  const buf = encoder.encode(bytes);
  predict.applyInput(buf);
  if (!connection.sendBinary(buf)) {
  }
}

// --- Initialize layers ---
status.init();

render.init({
  output: outputEl,
  termWrap,
  // Keep the helper textarea + IME composition view glued to the
  // cursor on every render so iOS keyboard focus and IME candidate
  // popups target the right area. Also re-render the predicted-cursor
  // overlay so its position stays consistent with the just-drawn
  // server cursor.
  onCursorMove: () => {
    composition.positionCompositionView();
    const p = predict.get();
    render.setPredictedCursor(p.row, p.col, p.active);
  },
});
render.updateFontMetrics();

// predict redraws on every change to its predicted-cursor state.
predict.subscribe(() => {
  const p = predict.get();
  render.setPredictedCursor(p.row, p.col, p.active);
});

composition.init({
  textarea: input,
  compositionView: compositionViewEl,
  getCursorPx: render.getCursorPx,
  send,
});

scroll.init({
  scrollEl: termWrap,
  onUserScrollChange(scrolledUp) {
    // Toggle .scrolled-up on the toolbar so CSS can show/hide the
    // scroll-bottom button and grow the pill vertically.
    const toolbar = document.getElementById("key-toolbar");
    if (toolbar) toolbar.classList.toggle("scrolled-up", scrolledUp);
  },
});

connection.init({
  computeSize: render.computeSize,
  onMessage(msg) {
    if (msg.type === "screen") {
      render.handleScreen(msg);
      if (fontsLoaded) {
        const ld = document.getElementById("loading");
        if (ld) {
          ld.classList.add("fade");
          ld.addEventListener("transitionend", () => ld.remove());
        }
      }
      predict.onScreenFrame(msg.cursor[0], msg.cursor[1]);
    } else if (msg.type === "scroll") {
      render.handleScroll(msg);
    }
  },
  onOpen() {
    status.open();
    // Do NOT call render.resetScreen() here. resetScreen flips the
    // firstScreen flag, which causes the next screen frame to wipe
    // the entire #term-output DOM (including all scrollback above
    // the live viewport). On reconnect (e.g. iPad screen dim/wake)
    // that destroys history the user could otherwise scroll up to.
    // The server forces a full repaint via builder.Reset() on resume,
    // which the client renders as row replacements within the live
    // zone — scrollback DOM stays intact. The very first onOpen of
    // page load still wipes correctly because firstScreen defaults
    // to true at module load (only resetScrollback + resetScreen on
    // onServerRestart explicitly trigger a full reset).
    wsOpen = true;
    maybeSendFirstResize();
  },
  onConnecting() {
    status.reconnecting();
  },
  onClose() {
    status.closed();
  },
  onOutboxFull() {
    // The user kept typing through a long disconnect and we've
    // capped the buffer. Surface visibly so they don't keep typing
    // into the void.
    status.closed();
  },
  onServerRestart() {
    // Wipe the now-stale scrollback DOM and the live viewport so the
    // user doesn't see ghost input from the previous server boot;
    // the next screen frame from the fresh server populates the
    // viewport. Banner explains why.
    render.resetScrollback();
    render.resetScreen();
    status.restarted();
  },
});

// --- Input handling ---

// Single-character placeholder kept in the hidden textarea so iOS soft
// keyboards have something to "delete" when the user holds Backspace.
// iOS only fires repeating `input` events with
// inputType="deleteContentBackward" when the textarea has content to
// delete; with a perpetually empty textarea (the previous behaviour),
// holding Backspace deletes one char and stops because iOS sees
// nothing more to remove. The placeholder itself is invisible
// (textarea has opacity:0) and we strip it out of every send. NBSP
// chosen specifically so screen-reader announcement of the input
// state stays empty-ish rather than "space".
const INPUT_PLACEHOLDER = "\u00A0";

function resetInputPlaceholder(): void {
  input.value = INPUT_PLACEHOLDER;
  // Cursor at the end so the next typed char appends after the
  // placeholder rather than before.
  try {
    input.setSelectionRange(INPUT_PLACEHOLDER.length, INPUT_PLACEHOLDER.length);
  } catch {
    // Ignore browsers that throw on setSelectionRange against a hidden
    // input (some older WebKit builds).
  }
}

resetInputPlaceholder();

input.addEventListener("input", (e: Event) => {
  // While IME composition is in progress, the textarea fires `input`
  // events for each composing keystroke. composition.ts owns sending
  // the final composed text in compositionend; we must NOT send the
  // intermediate input value (it would duplicate the composition).
  if (composition.isComposing()) return;

  const ev = e as InputEvent;
  const inputType = ev.inputType;

  if (inputType === "deleteContentBackward") {
    // Handled by keydown — just re-pad the placeholder so iOS
    // key-repeat keeps firing (it needs content to delete).
    resetInputPlaceholder();
    return;
  } else if (inputType === "deleteContentForward") {
    resetInputPlaceholder();
    return;
  } else if (inputType === "deleteWordBackward") {
    resetInputPlaceholder();
    return;
  } else if (inputType === "deleteWordForward") {
    resetInputPlaceholder();
    return;
  } else if (typeof ev.data === "string" && ev.data.length > 0) {
    // insertText / insertFromPaste / insertReplacementText etc. The
    // `data` property carries exactly the new content; using it
    // sidesteps having to diff against the placeholder.
    send(ev.data);
  } else {
    // Fallback: anything in the textarea past the placeholder is new
    // content. Covers browsers that don't populate inputType / data
    // (older WebKit).
    const v = input.value;
    if (v.length > INPUT_PLACEHOLDER.length && v.startsWith(INPUT_PLACEHOLDER)) {
      send(v.slice(INPUT_PLACEHOLDER.length));
    } else if (v !== INPUT_PLACEHOLDER && v.length > 0) {
      send(v);
    }
  }
  resetInputPlaceholder();
});

// Focus state on the terminal element, for CSS targeting (e.g. dimming
// the cursor when focus is elsewhere). Pattern from xterm.js.
input.addEventListener("focus", () => {
  termWrap.classList.add("focus");
});
input.addEventListener("blur", () => {
  // Restore the placeholder so the held-Backspace iOS path stays
  // primed for the next focus. Also clears any leftover screen-reader
  // text (xterm.js convention).
  resetInputPlaceholder();
  termWrap.classList.remove("focus");
});

// Keydown handler — attached to both the contenteditable output (for
// desktop) and the textarea (for iOS virtual keyboard).
function handleKeydown(ev: KeyboardEvent): void {
  // While composing (IME), let the browser pump composition events;
  // keydown bytes during composition would duplicate the composed text.
  if (composition.isComposing()) return;

  // Ctrl+Shift+C / Ctrl+Shift+V — desktop clipboard shortcuts. Handled
  // before the generic mapper because they take browser-side selection
  // and clipboard, not server-bound key sequences.
  if (ev.ctrlKey && ev.shiftKey && !ev.altKey && !ev.metaKey) {
    if (ev.code === "KeyC") {
      const sel = window.getSelection()?.toString();
      if (sel && navigator.clipboard?.writeText) {
        void navigator.clipboard.writeText(sel).catch(() => { /* ignore */ });
      }
      ev.preventDefault();
      return;
    }
    if (ev.code === "KeyV") {
      if (navigator.clipboard?.readText) {
        navigator.clipboard.readText().then((text) => {
          send(bracketTextForPaste(prepareTextForTerminal(text)));
        }).catch(() => { /* ignore */ });
      }
      ev.preventDefault();
      return;
    }
  }

  const result = mapKeyboardEvent(ev);
  switch (result.kind) {
    case "send":
      ev.preventDefault();
      send(result.bytes);
      return;
    case "scroll-up": {
      ev.preventDefault();
      const h = termWrap.clientHeight;
      termWrap.scrollTop = Math.max(0, termWrap.scrollTop - h);
      return;
    }
    case "scroll-down": {
      ev.preventDefault();
      const h = termWrap.clientHeight;
      termWrap.scrollTop = Math.min(termWrap.scrollHeight, termWrap.scrollTop + h);
      return;
    }
    case "ignore":
      // Defer to the browser; the `input` listener above will pick up
      // any printable character produced by the keystroke.
      return;
  }
}
outputEl.addEventListener("keydown", handleKeydown);
input.addEventListener("keydown", handleKeydown);

// --- Focus strategy ---
// The terminal output div is contenteditable — it receives keyboard
// events directly AND allows native text selection. No hidden textarea
// conflict. The textarea is kept only for iOS virtual keyboard (which
// needs a real input element to trigger). On touch tap we focus the
// textarea; on all other interactions the output div handles input.

// Prevent contenteditable from actually modifying the DOM, but
// capture the typed text and send it to the terminal.
outputEl.addEventListener("beforeinput", (e) => {
  if (e.inputType.startsWith("insertComposition")) return;
  e.preventDefault();

  // Handle typed text (not delete/backspace/enter — those are handled by keydown).
  if (e.inputType === "insertText" && e.data) {
    send(e.data);
  } else if (e.inputType === "insertFromPaste" && e.data) {
    send(bracketTextForPaste(prepareTextForTerminal(e.data)));
  }
});

// Focus the output div on page load and after visibility changes.
function focusTerminal(): void {
  outputEl.focus({ preventScroll: true });
}

// Touch tap: focus the hidden textarea to trigger iOS keyboard.
let lastPointerType = "mouse";
termWrap.addEventListener("pointerdown", (e) => {
  lastPointerType = e.pointerType;
}, { passive: true });
termWrap.addEventListener("click", () => {
  if (lastPointerType === "touch") {
    const sel = window.getSelection();
    if (sel && sel.toString().length > 0) return;
    input?.focus({ preventScroll: true });
  } else {
    // Mouse/trackpad: focus the contenteditable for keyboard input.
    // Selection is preserved because we're focusing the same element
    // the selection lives in.
    focusTerminal();
  }
});

// --- Viewport ---
// Centralized handling of iOS keyboard, window resize, and font-load
// reflows. Whenever the viewport settles, font metrics are remeasured
// and a resize is sent to the server. Snap-back-to-bottom (if the user
// was at the bottom before the transition) is handled inside
// viewport.ts.
// Send the first resize only when BOTH fonts are loaded AND the WS is
// open. Either can happen first depending on network/cache conditions.
let fontsLoaded = false;
let wsOpen = false;

function maybeSendFirstResize(): void {
  if (!fontsLoaded || !wsOpen) return;
  render.updateFontMetrics();
  connection.sendResize(); // sends only if size changed
  const sz = render.computeSize();
  predict.setDimensions(sz.cols, sz.rows);
}

// Only wait for the Regular weight — it determines cell size.
// Bold/Italic load lazily when first used; style pop is barely noticeable.
const regularFont = document.fonts
  ? document.fonts.load('14px "MonaspiceNe NFM"').then(() => {})
  : Promise.resolve();
void regularFont.then(() => {
  fontsLoaded = true;
  requestAnimationFrame(() => maybeSendFirstResize());
});

// ... viewport init
viewport.init({
  termWrap,
  onSettled() {
    render.updateFontMetrics();
    // Only send resize if fonts are loaded — otherwise we'd send the
    // wrong size (fallback font metrics) which causes the snap.
    if (fontsLoaded) {
      connection.sendResize();
    }
    composition.positionCompositionView();
    const sz = render.computeSize();
    predict.setDimensions(sz.cols, sz.rows);
  },
});

// Connect immediately — the WS open triggers maybeSendFirstResize.
render.updateFontMetrics();
composition.positionCompositionView();
connection.connect();
focusTerminal();

document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") {
    connection.reconnectNow();
    focusTerminal();
  }
});
window.addEventListener("pageshow", () => {
  connection.reconnectNow();
  focusTerminal();
});

// --- Scroll-to-bottom (inside toolbar grid) ---
const scrollBtn = document.getElementById("scroll-bottom");
if (scrollBtn) {
  scrollBtn.addEventListener("click", () => {
    scroll.scrollToBottom();
  });
}

// --- Context menu ---
const ctxMenu = document.getElementById("ctx-menu")!;

// iOS Safari shows a system "Paste" permission toast every time
// navigator.clipboard.readText() is called — by design, and unavoidable
// from JavaScript. iOS users get a one-tap paste via the native
// long-press callout (which routes through the textarea's paste event
// handler in composition.ts without ever calling readText), so we omit
// the Paste button from our custom menu on iOS to steer them there.
const isIOS = /iPad|iPhone|iPod/.test(navigator.userAgent) ||
  (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);

function hideCtxMenu(): void {
  ctxMenu.classList.remove("visible");
  ctxMenu.innerHTML = "";
}

function showCtxMenu(x: number, y: number): void {
  hideCtxMenu();

  const sel = window.getSelection()?.toString();
  if (sel) {
    const copyBtn = document.createElement("button");
    copyBtn.textContent = "Copy";
    copyBtn.addEventListener("click", () => {
      void navigator.clipboard.writeText(sel).catch(() => { /* ignore */ });
      hideCtxMenu();
    });
    ctxMenu.appendChild(copyBtn);
  }

  const selectAllBtn = document.createElement("button");
  selectAllBtn.textContent = "Select All";
  selectAllBtn.addEventListener("click", () => {
    const s = window.getSelection();
    if (s) { s.selectAllChildren(outputEl); }
    hideCtxMenu();
  });
  ctxMenu.appendChild(selectAllBtn);

  if (!isIOS || lastPointerType !== "touch") {
    const pasteBtn = document.createElement("button");
    pasteBtn.textContent = "Paste";
    pasteBtn.addEventListener("click", () => {
      if (navigator.clipboard?.readText) {
        navigator.clipboard.readText().then((text) => {
          send(bracketTextForPaste(prepareTextForTerminal(text)));
        }).catch(() => { /* ignore */ });
      }
      hideCtxMenu();
    });
    ctxMenu.appendChild(pasteBtn);
  }

  // Don't show an empty menu (iOS without selection has nothing to offer).
  if (ctxMenu.childElementCount === 0) return;

  ctxMenu.style.left = `${x}px`;
  ctxMenu.style.top = `${y}px`;
  ctxMenu.classList.add("visible");
}

termWrap.addEventListener("contextmenu", (e: MouseEvent) => {
  e.preventDefault();
  showCtxMenu(e.clientX, e.clientY);
});

// Long-press to trigger the same menu on touch devices that don't
// already provide a native callout. iOS Safari shows its own
// long-press selection callout (Select / Copy / Paste) which fully
// covers the use case AND would race our custom menu (two stacked
// menus appear if the user long-presses an existing selection), so
// we skip our timer on iOS and let the native callout do its job.
// Android Chrome fires `contextmenu` on long-press release, but the
// timer also helps on browsers that don't, e.g. some Windows touch
// devices.
const LONG_PRESS_MS = 500;
const LONG_PRESS_MOVE_THRESHOLD_PX = 10;
let longPressTimer = 0;
let longPressOrigin = { x: 0, y: 0 };

if (!isIOS) {
  termWrap.addEventListener("touchstart", (e: TouchEvent) => {
    if (e.touches.length !== 1) {
      if (longPressTimer) { clearTimeout(longPressTimer); longPressTimer = 0; }
      return;
    }
    const t = e.touches[0]!;
    longPressOrigin = { x: t.clientX, y: t.clientY };
    longPressTimer = window.setTimeout(() => {
      longPressTimer = 0;
      showCtxMenu(longPressOrigin.x, longPressOrigin.y);
    }, LONG_PRESS_MS);
  }, { passive: true });

  termWrap.addEventListener("touchmove", (e: TouchEvent) => {
    if (!longPressTimer || e.touches.length !== 1) return;
    const t = e.touches[0]!;
    const dx = t.clientX - longPressOrigin.x;
    const dy = t.clientY - longPressOrigin.y;
    if (dx * dx + dy * dy > LONG_PRESS_MOVE_THRESHOLD_PX * LONG_PRESS_MOVE_THRESHOLD_PX) {
      clearTimeout(longPressTimer);
      longPressTimer = 0;
    }
  }, { passive: true });

  termWrap.addEventListener("touchend", () => {
    if (longPressTimer) { clearTimeout(longPressTimer); longPressTimer = 0; }
  }, { passive: true });

  termWrap.addEventListener("touchcancel", () => {
    if (longPressTimer) { clearTimeout(longPressTimer); longPressTimer = 0; }
  }, { passive: true });
}

document.addEventListener("click", () => { hideCtxMenu(); });

// --- Mobile key toolbar ---
const keyToolbar = document.getElementById("key-toolbar");
if (keyToolbar) {
  requestAnimationFrame(() => requestAnimationFrame(() => {
    keyToolbar.classList.remove("no-transition");
  }));
  const toggleBtn = document.getElementById("kb-toggle");
  toggleBtn?.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    keyToolbar.classList.toggle("collapsed");
  });

  const keyMap: Record<string, string> = {
    "kb-up": "\x1b[A",
    "kb-down": "\x1b[B",
    "kb-left": "\x1b[D",
    "kb-right": "\x1b[C",
    "kb-esc": "\x1b",
    "kb-tab": "\t",
    "kb-enter": "\r",
    "kb-ctrlc": "\x03",
  };

  for (const [id, seq] of Object.entries(keyMap)) {
    document.getElementById(id)?.addEventListener("pointerdown", (e) => {
      e.preventDefault();
      send(seq);
    });
  }
}

// --- Copy feedback toast ---
document.addEventListener("copy", () => {
  status.toast("Copied");
});
