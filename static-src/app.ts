// web-terminal-kiro client entry point.
//
// All terminal behavior lives in the shared packages: the
// @cplieger/web-terminal-engine engine (render / scroll / connection / keyboard)
// and the @cplieger/web-terminal-ui reference UI (the modular kernel plus opt-in
// features). web-terminal-kiro is the thinnest possible consumer: createTerminal builds the
// whole terminal UI inside the #terminal root element with the agent-shell
// feature set (presetAgentTabbed: tabs + activity monitor + touch toolbar +
// context menu + clipboard + scroll-to-bottom + predictive echo + connection
// banner + animations). web-terminal-kiro is an agent shell, so it wants the activity
// monitor (per-tab working/done/needs-input dots); a generic terminal would use
// the plain presetTabbed, which is label-only. Each browser tab drives its own
// independent kiro-cli chat session
// over the shared server; kiro-cli's TUI is rendered verbatim through the raw PTY
// stream.
//
// The session WebSocket ("/ws") and font (Monaspace) use createTerminal's
// defaults and are left implicit. The options passed are `features` (the agent
// preset), `theme` (web-terminal-kiro's purple tokens), and -- only when present --
// `loading`, the overlay element createTerminal fades out once the first frame
// renders.

import { createTerminal } from "@cplieger/web-terminal-ui";
import { presetAgentTabbed } from "@cplieger/web-terminal-ui/presets";

// Reveal the #loading overlay as an assertive alert with a fatal message.
// remove("fade") undoes any fade-out createTerminal began; on the missing-root
// path createTerminal never ran, so the remove is a harmless no-op.
function showFatal(overlay: HTMLElement, message: string): void {
  overlay.classList.remove("fade");
  // index.html names the overlay "Loading" (aria-label); drop it so the
  // alert's accessible name doesn't contradict the fatal message it now shows.
  overlay.removeAttribute("aria-label");
  overlay.setAttribute("role", "alert");
  overlay.setAttribute("aria-live", "assertive");
  overlay.textContent = message;
  // manifest.json declares display: standalone, so an installed PWA has no browser
  // chrome; "reload the page" needs an in-page affordance (Vercel: no dead ends).
  const reload = document.createElement("button");
  reload.type = "button";
  reload.textContent = "Reload";
  reload.addEventListener("click", () => {
    window.location.reload();
  });
  overlay.append(" ", reload);
  // Move focus to the recovery CTA: the page content is gone, the alert has
  // announced, and Reload is the only actionable element left (APG alert
  // pattern advises against interactive content in an un-focus-managed alert;
  // alertdialog's focus move is the matching treatment).
  reload.focus();
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
      "Web Terminal for Kiro failed to start. Reload the page; if this persists the app was built incorrectly.",
    );
  }
  throw new Error("web-terminal-kiro: missing #terminal root element");
}
try {
  createTerminal(root, {
    features: presetAgentTabbed(),
    // web-terminal-kiro's purple theme (the consumer "settings"; the UI library ships the
    // neutral defaults). Recolors hovered/active tabs, the accent icons (the
    // mobile "+", the toggled keyboard button), and the tab activity dots
    // (--status-*, below). Since web-terminal-ui v4 all tokens
    // live on .wt-root -- the element the theme is applied to -- so the library's
    // --tab-active-border derivation (the fill lightened + slightly desaturated)
    // already follows an overridden fill; the explicit re-declaration below is a
    // deliberate pin of that same formula, kept so the edge stays low-saturation
    // even if the library's derivation formula changes.
    theme: {
      "--accent": "hsl(263.1683 100% 80%)",
      "--tab-hover-bg": "hsl(263.1683 100% 80% / 16%)",
      "--tab-active-bg": "hsl(263.1683 100% 80% / 32%)",
      "--tab-active-border": "color-mix(in oklch, var(--tab-active-bg), var(--text) 25%)",
      "--tab-active-fg": "#fff",
      // Tab activity-dot vocabulary (replaces the library defaults; ui >= the
      // release that tokenized the dots -- older bundles ignore these and keep
      // the defaults): violet = thinking, green = done, yellow = action
      // required. One declared family -- 78% lightness / 0.15 chroma, only the
      // hue carries the state -- sitting at the pastel accent's own level
      // (#c099ff is ~oklch(76% 0.13 296deg)). The violet hue's sRGB ceiling at
      // 78% L is C~0.132, so browsers gamut-map the declared 0.15 down to the
      // max-chroma pastel violet (~#c4a3ff); that clamp is deliberate ("the
      // most saturated violet available at the family's lightness"). Green
      // (~#67d283) and yellow (~#d6b529) are in gamut and render as declared.
      // Hue alone never carries state (pulse/ring/shape per WCAG 1.4.1):
      // working and input share one ringed silhouette -- live pulses, blocked
      // is frozen -- and done stays the bare disc. Both rings derive from
      // their own token inside the library CSS.
      "--status-working": "oklch(78% 0.15 300deg)",
      "--status-done": "oklch(78% 0.15 150deg)",
      "--status-input": "oklch(78% 0.15 95deg)",
    },
    ...(loading ? { loading } : {}),
  });
} catch (e) {
  if (loading) {
    showFatal(loading, "Failed to start the terminal. Reload the page to retry.");
  }
  throw e;
}
