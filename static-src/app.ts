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
// The session WebSocket ("/ws") and font (Monaspace) use createTerminal's
// defaults and are left implicit. The options passed are `features` (the agent
// preset), `theme` (vibecli's purple tokens), and -- only when present --
// `loading`, the overlay element createTerminal fades out once the first frame
// renders.

import { createTerminal } from "@cplieger/web-terminal-ui";
import { presetAgentTabbed } from "@cplieger/web-terminal-ui/presets";

// Reveal the #loading overlay as an assertive alert with a fatal message.
// remove("fade") undoes any fade-out createTerminal began; on the missing-root
// path createTerminal never ran, so the remove is a harmless no-op.
function showFatal(overlay: HTMLElement, message: string): void {
  overlay.classList.remove("fade");
  overlay.setAttribute("role", "alert");
  overlay.setAttribute("aria-live", "assertive");
  overlay.textContent = message;
}

const loading = document.getElementById("loading");
const root = document.getElementById("terminal");
if (!root) {
  // Surface the failure on the page, not just the console: createTerminal (which
  // fades the #loading overlay out on first frame) is never reached on this path,
  // so without this the user is left on a stuck loading screen with no explanation.
  if (loading) {
    showFatal(
      loading,
      "vibecli failed to start. Reload the page; if this persists the app was built incorrectly.",
    );
  }
  throw new Error("vibecli: missing #terminal root element");
}
try {
  createTerminal(root, {
    features: presetAgentTabbed(),
    // vibecli's purple theme (the consumer "settings"; the UI library ships the
    // neutral defaults). Recolors hovered/active tabs and the accent icons (the
    // mobile "+", the toggled keyboard button). The active-tab border is set
    // explicitly: the library derives its default from --accent, but that
    // derivation is resolved once at :root against the light-blue default, so
    // overriding --accent alone leaves the border light blue — it must be
    // overridden here too (a lighter/brighter purple than the 32% fill, matching
    // the library's own accent-90% edge).
    theme: {
      "--accent": "hsl(263.1683 100% 80%)",
      "--tab-hover-bg": "hsl(263.1683 100% 80% / 16%)",
      "--tab-active-bg": "hsl(263.1683 100% 80% / 32%)",
      "--tab-active-border": "hsl(263.1683 100% 80% / 90%)",
      "--tab-active-fg": "#fff",
    },
    ...(loading ? { loading } : {}),
  });
} catch (e) {
  if (loading) {
    showFatal(loading, "Failed to start the terminal. Reload the page to retry.");
  }
  throw e;
}
