// vibecli client entry point: acquires DOM refs, initializes the render
// and connection layers, and wires up input + UI controls.

import { render, keyboard, scroll, connection } from "@cplieger/vterm";
import * as viewport from "./viewport.js";
import * as composition from "./composition.js";
import * as status from "./status.js";
import * as predict from "./predict.js";

const { mapKeyboardEvent, bracketTextForPaste, prepareTextForTerminal } = keyboard;

// --- DOM refs ---
// eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- element guaranteed in index.html
const outputEl = document.getElementById("term-output")!;
// eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- element guaranteed in index.html
const termWrap = document.getElementById("term")!;
const input = document.getElementById("term-input") as HTMLTextAreaElement;
// eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- element guaranteed in index.html
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
    /* connection not ready — drop silently */
  }
}

// --- Sticky Ctrl modifier ---
// The iOS virtual keyboard has no Ctrl key, so control sequences
// (Ctrl+C, Ctrl+L = clear screen, Ctrl+X, ...) are otherwise
// unreachable on touch. The toolbar's Ctrl button arms a one-shot
// modifier: tap it, then tap a letter on the virtual keyboard and that
// keystroke is sent as its C0 control byte. Auto-disarms after one
// printable character.
let ctrlArmed = false;
const ctrlBtn = document.getElementById("kb-ctrl");

function setCtrlArmed(on: boolean): void {
  ctrlArmed = on;
  ctrlBtn?.classList.toggle("armed", on);
  ctrlBtn?.setAttribute("aria-pressed", on ? "true" : "false");
}

// Map one printable character to its Ctrl+<char> C0 control byte,
// mirroring the Ctrl handling in @cplieger/vterm's keyboard mapper.
function ctrlByteFor(ch: string): string | null {
  const code = ch.toLowerCase().charCodeAt(0);
  if (code >= 97 && code <= 122) {
    return String.fromCharCode(code - 96); // a–z → 0x01–0x1a
  }
  switch (ch) {
    case " ":
    case "@":
      return "\x00";
    case "[":
      return "\x1b";
    case "\\":
      return "\x1c";
    case "]":
      return "\x1d";
    case "^":
      return "\x1e";
    case "_":
      return "\x1f";
    case "?":
      return "\x7f";
    default:
      return null;
  }
}

// Apply a one-shot armed Ctrl to freshly-typed text: a single printable
// character becomes its control byte; longer input (paste) just disarms
// and passes through unchanged.
function applyStickyCtrl(data: string): string {
  if (!ctrlArmed) {
    return data;
  }
  setCtrlArmed(false);
  if (data.length === 1) {
    return ctrlByteFor(data) ?? data;
  }
  return data;
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
    if (toolbar) {
      toolbar.classList.toggle("scrolled-up", scrolledUp);
    }
  },
});

connection.init({
  computeSize: render.computeSize,
  getHaveThrough: render.getHighestIndex,
  onResumeBounds: render.noteResumeBounds,
  onMessage(msg) {
    if (msg.type === "screen") {
      render.handleScreen(msg);
      if (fontsLoaded) {
        const ld = document.getElementById("loading");
        if (ld) {
          ld.classList.add("fade");
          ld.addEventListener("transitionend", () => {
            ld.remove();
          });
        }
      }
      predict.onScreenFrame(msg.cursor[0], msg.cursor[1], msg.cursorHidden);
    } else if (msg.type === "scroll") {
      render.handleScroll(msg);
    } else if (msg.type === "title") {
      document.title = msg.title;
    } else if (msg.type === "modes") {
      render.updateReverseVideo();
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
    predict.reset();
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
  if (composition.isComposing()) {
    return;
  }

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
    //
    // iOS Safari can deliver U+00A0 (NBSP) instead of U+0020 for the
    // spacebar/autocorrect. Normalize so kiro-cli receives an honest
    // space byte. (Native paste is bracketed in composition.ts's paste
    // handler before this fires; the insertFromPaste data here is a
    // fallback for browsers that don't raise a separate paste event.)
    send(applyStickyCtrl(ev.data.replace(/\u00A0/g, " ")));
  } else {
    // Fallback: anything in the textarea past the placeholder is new
    // content. Covers browsers that don't populate inputType / data
    // (older WebKit).
    const v = input.value;
    if (v.length > INPUT_PLACEHOLDER.length && v.startsWith(INPUT_PLACEHOLDER)) {
      send(applyStickyCtrl(v.slice(INPUT_PLACEHOLDER.length).replace(/\u00A0/g, " ")));
    } else if (v !== INPUT_PLACEHOLDER && v.length > 0) {
      send(applyStickyCtrl(v.replace(/\u00A0/g, " ")));
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
  if (composition.isComposing()) {
    return;
  }

  // Ctrl+Shift+C / Ctrl+Shift+V — desktop clipboard shortcuts. Handled
  // before the generic mapper because they take browser-side selection
  // and clipboard, not server-bound key sequences.
  if (ev.ctrlKey && ev.shiftKey && !ev.altKey && !ev.metaKey) {
    if (ev.code === "KeyC") {
      const sel = window.getSelection()?.toString();
      if (sel) {
        void navigator.clipboard.writeText(sel).catch(() => {
          /* ignore */
        });
      }
      ev.preventDefault();
      return;
    }
    if (ev.code === "KeyV") {
      navigator.clipboard
        .readText()
        .then((text) => {
          send(bracketTextForPaste(prepareTextForTerminal(text)));
        })
        .catch(() => {
          /* ignore */
        });
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
// The textarea is the single keyboard target (desktop and touch alike);
// #term-output is display-only and never focused.
input.addEventListener("keydown", handleKeydown);

// --- Focus strategy ---
// One element, one job. #term-output is display + native selection only
// and is NEVER focused; the textarea owns the keyboard, the local typing
// buffer, and IME. Because the editable element is never the scroll
// content, the first touch-drag scrolls instead of placing a caret (bug
// 6) and a tap on a sparse screen still lands on the full-viewport scroll
// surface (bug 7); because the display is not re-rendered as editable, a
// selection survives a redraw (bug 1). Typed text, paste, and IME all
// arrive through the textarea's own input / paste / composition listeners
// (the latter two live in composition.ts).

// Focus the textarea on page load and after visibility changes.
function focusTerminal(): void {
  input.focus({ preventScroll: true });
}

// Touch tap: focus the hidden textarea to trigger iOS keyboard.
// On iOS Safari, focusing a hidden textarea from a `click` handler
// only opens the virtual keyboard if the click event arrives within
// iOS's user-gesture window. During heavy DOM mutations (kiro
// streaming), the click event timing slips past that window — symptom:
// "while kiro is thinking I can't open the keyboard; only when the
// turn ends does tap work." `pointerup` fires synchronously with the
// user's finger lift and captures the gesture before iOS layout work
// can defer it, so focus() lands while the gesture is still "live".
// We track the start position from pointerdown and only treat the
// pointerup as a tap (vs scroll) when total movement is sub-threshold.
let lastPointerType = "mouse";
let pointerDownX = 0;
let pointerDownY = 0;
const TAP_MOVEMENT_PX = 10;
termWrap.addEventListener(
  "pointerdown",
  (e) => {
    lastPointerType = e.pointerType;
    pointerDownX = e.clientX;
    pointerDownY = e.clientY;
  },
  { passive: true },
);
termWrap.addEventListener(
  "pointerup",
  (e) => {
    if (e.pointerType !== "touch") {
      return; // mouse/trackpad handled by the click listener below
    }
    const dx = Math.abs(e.clientX - pointerDownX);
    const dy = Math.abs(e.clientY - pointerDownY);
    if (dx > TAP_MOVEMENT_PX || dy > TAP_MOVEMENT_PX) {
      return; // user was scrolling, not tapping
    }
    const sel = window.getSelection();
    if (sel && sel.toString().length > 0) {
      return; // preserve selection (long-tap selected text)
    }
    // Synchronous focus inside pointerup keeps us inside iOS's
    // user-gesture window even when the streaming flush queue is busy.
    input.focus({ preventScroll: true });
  },
  { passive: true },
);
termWrap.addEventListener("click", (e) => {
  // Terminal links: URLs detected by render.ts's linkifySpans are wrapped
  // in <a class="term-link" target="_blank">. In a contenteditable element
  // the browser treats link clicks as cursor-placement, not navigation.
  // Intercept and open explicitly.
  const link = (e.target as HTMLElement).closest<HTMLAnchorElement>(".term-link");
  if (link) {
    e.preventDefault();
    window.open(link.href, "_blank", "noopener,noreferrer");
    return;
  }

  if (lastPointerType === "touch") {
    // Touch path is handled by pointerup above. The click listener
    // is kept so non-pointer-event environments (very old iOS
    // builds without PointerEvent support) still get a focus hook.
    return;
  }
  // Mouse/trackpad: a plain click focuses the textarea so the user can
  // type. Never steal focus mid-selection though — a drag-select ends
  // with a click, and grabbing focus would collapse the selection the
  // user just made and still wants to copy.
  const sel = window.getSelection();
  if (sel && sel.toString().length > 0) {
    return;
  }
  focusTerminal();
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
  if (!fontsLoaded || !wsOpen) {
    return;
  }
  render.updateFontMetrics();
  connection.sendResize(); // sends only if size changed
  const sz = render.computeSize();
  predict.setDimensions(sz.cols, sz.rows);
}

// Only wait for the Regular weight — it determines cell size.
// Bold/Italic load lazily when first used; style pop is barely noticeable.
const regularFont = document.fonts.load('14px "MonaspiceNe NFM"').then(() => {
  /* discard result */
});
void regularFont.then(() => {
  fontsLoaded = true;
  requestAnimationFrame(() => {
    maybeSendFirstResize();
  });
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
// Network came back (cellular↔wifi handoff, tunnel reconnect): re-establish
// at once rather than waiting out the reconnect backoff. reconnectNow tears
// down any zombie socket first, and resume-by-index backfills what was missed.
window.addEventListener("online", () => {
  connection.reconnectNow();
});

// --- Scroll-to-bottom (inside toolbar grid) ---
const scrollBtn = document.getElementById("scroll-bottom");
if (scrollBtn) {
  // pointerdown (like the other toolbar keys) so touch devices enter
  // :active and show press feedback; preventDefault keeps focus on the
  // terminal. click is kept for keyboard activation of the desktop FAB.
  // scrollToBottom() is idempotent, so the pointerdown+click pair on a
  // mouse press is harmless.
  scrollBtn.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    scroll.scrollToBottom();
  });
  scrollBtn.addEventListener("click", () => {
    scroll.scrollToBottom();
  });
}

// --- Context menu ---
// eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- element guaranteed in index.html
const ctxMenu = document.getElementById("ctx-menu")!;

// iOS Safari shows a system "Paste" permission toast every time
// navigator.clipboard.readText() is called — by design, and unavoidable
// from JavaScript. iOS users get a one-tap paste via the native
// long-press callout (which routes through the textarea's paste event
// handler in composition.ts without ever calling readText), so we omit
// the Paste button from our custom menu on iOS to steer them there.
const isIOS =
  /iPad|iPhone|iPod/.test(navigator.userAgent) ||
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
      void navigator.clipboard.writeText(sel).catch(() => {
        /* ignore */
      });
      hideCtxMenu();
    });
    ctxMenu.appendChild(copyBtn);
  }

  const selectAllBtn = document.createElement("button");
  selectAllBtn.textContent = "Select All";
  selectAllBtn.addEventListener("click", () => {
    const s = window.getSelection();
    if (s) {
      s.selectAllChildren(outputEl);
    }
    hideCtxMenu();
  });
  ctxMenu.appendChild(selectAllBtn);

  if (!isIOS || lastPointerType !== "touch") {
    const pasteBtn = document.createElement("button");
    pasteBtn.textContent = "Paste";
    pasteBtn.addEventListener("click", () => {
      navigator.clipboard
        .readText()
        .then((text) => {
          send(bracketTextForPaste(prepareTextForTerminal(text)));
        })
        .catch(() => {
          /* ignore */
        });
      hideCtxMenu();
    });
    ctxMenu.appendChild(pasteBtn);
  }

  // Don't show an empty menu (iOS without selection has nothing to offer).
  if (ctxMenu.childElementCount === 0) {
    return;
  }

  // Make it visible (so it has measurable dimensions) then clamp it
  // inside the viewport, so it never opens off-screen near the right or
  // bottom edge — the off-screen-callout half of bug 1. position:fixed
  // means x/y are already viewport coordinates. Setting left/top after
  // adding the class is flash-free: it is all one synchronous task, so
  // the browser paints only the clamped position.
  ctxMenu.classList.add("visible");
  const margin = 8;
  const left = Math.max(margin, Math.min(x, window.innerWidth - ctxMenu.offsetWidth - margin));
  const top = Math.max(margin, Math.min(y, window.innerHeight - ctxMenu.offsetHeight - margin));
  ctxMenu.style.left = `${left}px`;
  ctxMenu.style.top = `${top}px`;
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
  termWrap.addEventListener(
    "touchstart",
    (e: TouchEvent) => {
      if (e.touches.length !== 1) {
        if (longPressTimer) {
          clearTimeout(longPressTimer);
          longPressTimer = 0;
        }
        return;
      }
      // eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- touches.length checked above
      const t = e.touches[0]!;
      longPressOrigin = { x: t.clientX, y: t.clientY };
      longPressTimer = window.setTimeout(() => {
        longPressTimer = 0;
        showCtxMenu(longPressOrigin.x, longPressOrigin.y);
      }, LONG_PRESS_MS);
    },
    { passive: true },
  );

  termWrap.addEventListener(
    "touchmove",
    (e: TouchEvent) => {
      if (!longPressTimer || e.touches.length !== 1) {
        return;
      }
      // eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- touches.length checked above
      const t = e.touches[0]!;
      const dx = t.clientX - longPressOrigin.x;
      const dy = t.clientY - longPressOrigin.y;
      if (dx * dx + dy * dy > LONG_PRESS_MOVE_THRESHOLD_PX * LONG_PRESS_MOVE_THRESHOLD_PX) {
        clearTimeout(longPressTimer);
        longPressTimer = 0;
      }
    },
    { passive: true },
  );

  termWrap.addEventListener(
    "touchend",
    () => {
      if (longPressTimer) {
        clearTimeout(longPressTimer);
        longPressTimer = 0;
      }
    },
    { passive: true },
  );

  termWrap.addEventListener(
    "touchcancel",
    () => {
      if (longPressTimer) {
        clearTimeout(longPressTimer);
        longPressTimer = 0;
      }
    },
    { passive: true },
  );
}

document.addEventListener("click", () => {
  hideCtxMenu();
});

// --- Mobile key toolbar ---
const keyToolbar = document.getElementById("key-toolbar");
if (keyToolbar) {
  requestAnimationFrame(() =>
    requestAnimationFrame(() => {
      keyToolbar.classList.remove("no-transition");
    }),
  );
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
  };

  for (const [id, seq] of Object.entries(keyMap)) {
    document.getElementById(id)?.addEventListener("pointerdown", (e) => {
      e.preventDefault();
      setCtrlArmed(false);
      send(seq);
    });
  }

  // Sticky Ctrl: tap to arm/disarm. preventDefault keeps focus on the
  // terminal so the iOS virtual keyboard stays up for the next tap.
  ctrlBtn?.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    setCtrlArmed(!ctrlArmed);
  });
}

// --- Copy feedback toast ---
document.addEventListener("copy", () => {
  status.toast("Copied");
});
