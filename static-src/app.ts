// vibecli client entry point.
//
// All terminal behavior lives in the shared packages: the
// @cplieger/web-terminal-engine engine (render / scroll / connection / keyboard)
// and the @cplieger/web-terminal-ui reference UI (the modular kernel plus opt-in
// features). vibecli is the thinnest possible consumer: createTerminal builds the
// whole terminal UI inside the #terminal root element with the agent-shell
// feature set (presetAgentTabbed: tabs + activity monitor + touch toolbar +
// context menu + clipboard + scroll-to-bottom + predictive echo + connection
// banner + animations). vibecli is an agent shell, so it wants the activity
// monitor (per-tab working/done/needs-input dots); a generic terminal would use
// the plain presetTabbed, which is label-only. Each browser tab drives its own
// independent kiro-cli chat session
// over the shared server; kiro-cli's TUI is rendered verbatim through the raw PTY
// stream.
//
// The server serves the session WebSocket at "/ws" (createTerminal's default) and
// the bundled font is Monaspace (the fontReady default), so the only option
// passed is `loading`: the overlay element createTerminal fades out once the
// first frame renders.

import { createTerminal } from "@cplieger/web-terminal-ui";
import { presetAgentTabbed } from "@cplieger/web-terminal-ui/presets";

const root = document.getElementById("terminal");
if (!root) {
  // Surface the failure on the page, not just the console: createTerminal (which
  // fades the #loading overlay out on first frame) is never reached on this path,
  // so without this the user is left on a stuck loading screen with no explanation.
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
  createTerminal(root, {
    features: presetAgentTabbed(),
    // vibecli's purple theme (the consumer "settings"; the UI library ships the
    // neutral defaults). Recolors hovered/active tabs and the accent icons (the
    // mobile "+", the toggled keyboard button).
    theme: {
      "--accent": "hsl(263.1683 100% 80%)",
      "--tab-hover-bg": "hsl(263.1683 100% 80% / 16%)",
      "--tab-active-bg": "hsl(263.1683 100% 80% / 32%)",
      "--tab-active-fg": "#fff",
    },
    ...(loading ? { loading } : {}),
  });
} catch (e) {
  if (loading) {
    loading.classList.remove("fade");
    loading.setAttribute("role", "alert");
    loading.setAttribute("aria-live", "assertive");
    loading.textContent = "Failed to start the terminal. Reload the page to retry.";
  }
  throw e;
}
