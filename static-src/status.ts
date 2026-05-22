// Connection-state banner. Shows "Reconnecting…" / "Offline" / "Slow
// connection" feedback so users on flaky links understand why typing
// is unresponsive. Hidden by default; appears only when the
// connection state diverges from "open" for more than a brief window
// (debounced to avoid flicker on healthy networks).
//
// State machine:
//   open      → no banner.
//   closed    → "Reconnecting…" after CONNECTING_GRACE_MS, "Offline"
//               after several backoff failures.
//   reconnecting (during a connect retry) → "Reconnecting…".
//   slow      → "Slow connection" overlaid on top of "open" when the
//               server's pingstat reports a capped RTO. (Not wired
//               yet; placeholder for a future ping-RTT feedback path.)

const CONNECTING_GRACE_MS = 600;

type State = "open" | "connecting" | "reconnecting" | "offline" | "restarted";

let banner: HTMLElement | null = null;
let state: State = "connecting";
let stableTimer: ReturnType<typeof setTimeout> | null = null;
let consecutiveFailures = 0;

/** Mark the connection as open. Hides the banner. */
export function open(): void {
  consecutiveFailures = 0;
  setState("open", 0);
}

/** Mark a reconnect attempt in progress (after a close, before re-open). */
export function reconnecting(): void {
  if (document.getElementById("loading")) return;
  setState("reconnecting", CONNECTING_GRACE_MS);
}

/** Mark the connection as closed; shows banner after grace period. */
export function closed(): void {
  consecutiveFailures++;
  if (document.getElementById("loading")) return;
  setState(consecutiveFailures > 3 ? "offline" : "reconnecting", CONNECTING_GRACE_MS);
}

/** Surface that the server has restarted (epoch mismatch). The
 *  caller has already reset bytesSent/bytesAcked; this just shows
 *  a brief banner so the user knows old input may have been lost. */
export function restarted(): void {
  setState("restarted", 0);
  // Auto-clear the banner after a few seconds; "open" will re-arrive
  // when the next screen frame renders, but we don't want to wait.
  setTimeout(() => {
    if (state === "restarted") setState("open", 0);
  }, 4000);
}

function setState(next: State, delay: number): void {
  if (stableTimer !== null) {
    clearTimeout(stableTimer);
    stableTimer = null;
  }
  if (delay === 0) {
    state = next;
    applyState();
    return;
  }
  stableTimer = setTimeout(() => {
    stableTimer = null;
    state = next;
    applyState();
  }, delay);
}

/** Surface a brief transient message (e.g. "Copied") using the same
 *  banner system as connection status. Auto-clears after `ms`. */
export function toast(msg: string, ms = 3000): void {
  if (!banner) return;
  banner.textContent = msg;
  banner.dataset["state"] = "toast";
  banner.classList.add("visible");
  scheduleToastDismiss(ms);
}

let toastDismissTimer: ReturnType<typeof setTimeout> | null = null;
let toastDismissMs = 3000;

function scheduleToastDismiss(ms: number): void {
  toastDismissMs = ms;
  if (toastDismissTimer !== null) clearTimeout(toastDismissTimer);
  toastDismissTimer = setTimeout(dismissToast, ms);
}

function dismissToast(): void {
  toastDismissTimer = null;
  if (!banner) return;
  // Only clear if still showing a toast (not overridden by connection state).
  if (state === "open" && banner.dataset["state"] === "toast") {
    banner.classList.remove("visible");
    banner.textContent = "";
  }
}

export function init(): void {
  banner = document.getElementById("conn-banner");
  applyState();

  // Hover-pause: stop the auto-dismiss timer while the user hovers
  // (gives them time to read longer messages or click actions).
  banner?.addEventListener("mouseenter", () => {
    if (toastDismissTimer !== null) {
      clearTimeout(toastDismissTimer);
      toastDismissTimer = null;
    }
  });
  banner?.addEventListener("mouseleave", () => {
    if (banner?.dataset["state"] === "toast" && banner.classList.contains("visible")) {
      scheduleToastDismiss(toastDismissMs);
    }
  });

  // Escape-dismiss: pressing Escape while a toast is visible clears it.
  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape" && banner?.classList.contains("visible") && banner.dataset["state"] === "toast") {
      if (toastDismissTimer !== null) clearTimeout(toastDismissTimer);
      dismissToast();
    }
  });
}

function applyState(): void {
  if (!banner) return;
  switch (state) {
    case "open":
      banner.classList.remove("visible");
      banner.textContent = "";
      break;
    case "connecting":
    case "reconnecting":
      banner.textContent = "Reconnecting…";
      banner.dataset["state"] = "reconnecting";
      banner.classList.add("visible");
      break;
    case "offline":
      banner.textContent = "Offline";
      banner.dataset["state"] = "offline";
      banner.classList.add("visible");
      break;
    case "restarted":
      banner.textContent = "Server restarted — recent input may have been lost";
      banner.dataset["state"] = "restarted";
      banner.classList.add("visible");
      break;
  }
}
