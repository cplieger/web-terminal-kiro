// Viewport stability tracker.
//
// Coordinates iOS keyboard transitions, window resizes, font-load reflows,
// and ResizeObserver fires into a single "transition → settle" lifecycle
// so the rest of the app can act on stable viewport state without each
// piece reinventing its own debounce/suppress timers.
//
// Behavior:
//   1. Any viewport-affecting event marks "in transition".
//   2. While in transition: autoscroll is suppressed, input dimensions
//      may shift, no resize is sent to the server.
//   3. After SETTLE_MS of no further events, fire onSettled — the caller
//      then sends the final size, snaps to bottom if was-at-bottom, etc.

import * as scroll from "./scroll.js";

// Time to wait after the last viewport-changing event before declaring
// the viewport "settled". Long enough to bridge the iOS keyboard slide
// (~250ms) with margin for fonts and reflow.
const SETTLE_MS = 350;

// How long to suppress the scroll module's autoscroll during a transition.
const SUPPRESS_MS = 400;

let termWrap: HTMLElement;
let onSettled: ((wasAtBottom: boolean) => void) | null = null;
let inTransition = false;
let wasAtBottomAtStart = true;
let settleTimer: ReturnType<typeof setTimeout> | null = null;

function startTransition(): void {
  // Capture pre-transition scroll state at the *start* of the transition,
  // not on every event in the burst.
  if (!inTransition) {
    inTransition = true;
    wasAtBottomAtStart = !scroll.isUserScrolledUp();
  }
  scroll.suppressScroll(SUPPRESS_MS);

  if (settleTimer !== null) {clearTimeout(settleTimer);}
  settleTimer = setTimeout(() => {
    settleTimer = null;
    inTransition = false;
    if (wasAtBottomAtStart) {scroll.scrollToBottom();}
    if (onSettled) {onSettled(wasAtBottomAtStart);}
  }, SETTLE_MS);
}

export function init(opts: {
  termWrap: HTMLElement;
  onSettled: (wasAtBottom: boolean) => void;
}): void {
  termWrap = opts.termWrap;
  onSettled = opts.onSettled;

  // iOS soft keyboard. interactive-widget=resizes-content makes the
  // layout viewport shrink; we still apply a manual bottom inset as
  // fallback for older iOS / other mobile browsers.
  if (window.visualViewport) {
    const vv = window.visualViewport;
    const onChange = (): void => {
      const inset = Math.max(0, Math.round(window.innerHeight - vv.height));
      termWrap.style.bottom = inset > 0 ? `${inset}px` : "";
      // Expose keyboard inset as a CSS variable so other fixed elements
      // (e.g. the notification banner) can position above the keyboard.
      document.documentElement.style.setProperty("--kb-inset", `${inset}px`);
      startTransition();
    };
    vv.addEventListener("resize", onChange);
    vv.addEventListener("scroll", onChange);
    onChange();
  }

  // Term wrap dimension changes: window resize, font load, devtools dock.
  const ro = new ResizeObserver(startTransition);
  ro.observe(termWrap);
  window.addEventListener("resize", startTransition);

  // Orientation change on mobile. iOS Safari often emits the
  // window.resize event late or not at all on rotation while
  // visualViewport.resize fires reliably; screen.orientation.change is
  // the canonical signal that survives both. Modern browsers expose it
  // on screen.orientation; older Safari falls back to the deprecated
  // window.orientationchange event.
  const orientation = (screen as Screen & { orientation?: ScreenOrientation }).orientation;
  if (orientation && typeof orientation.addEventListener === "function") {
    orientation.addEventListener("change", startTransition);
  } else if ("onorientationchange" in window) {
    window.addEventListener("orientationchange", startTransition);
  }
}
