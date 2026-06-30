// vibecli client entry point.
//
// All terminal behavior lives in the shared packages: the
// @cplieger/web-terminal-engine engine (render / scroll / connection / keyboard)
// and the @cplieger/web-terminal-ui reference UI (the textarea input model,
// mobile key toolbar, context menu, IME, predictive echo, viewport handling).
// vibecli is the thinnest possible consumer: mount() builds the whole terminal
// UI inside the #terminal root element; kiro-cli's TUI is driven verbatim
// through the raw PTY stream.
//
// The server serves the WebSocket at "/ws" (mount's default), and the bundled
// font is Monaspace (the fontReady default), so the only option passed is
// `loading`: the overlay element that mount fades out once the first frame
// renders.

import { mount } from "@cplieger/web-terminal-ui";

const root = document.getElementById("terminal");
if (!root) {
  // Surface the failure on the page, not just the console: mount() (which fades
  // the #loading overlay out on first frame) is never reached on this path, so
  // without this the user is left on a stuck loading screen with no explanation.
  const overlay = document.getElementById("loading");
  if (overlay) {
    overlay.setAttribute("role", "alert");
    overlay.setAttribute("aria-live", "assertive");
    overlay.textContent =
      "vibecli failed to start. Reload the page; if this persists the app was built incorrectly.";
  }
  throw new Error("vibecli: missing #terminal root element");
}
const loading = document.getElementById("loading");
try {
  mount(root, loading ? { loading } : {});
} catch (e) {
  if (loading) {
    loading.classList.remove("fade");
    loading.setAttribute("role", "alert");
    loading.setAttribute("aria-live", "assertive");
    loading.textContent = "Failed to start the terminal. Reload the page to retry.";
  }
  throw e;
}
