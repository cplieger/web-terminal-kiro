// @vitest-environment happy-dom
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { beforeEach, describe, expect, it, vi } from "vitest";

// app.ts imports createTerminal from the UI package and presetAgentTabbed from
// its /presets subpath; mock both. presetAgentTabbed returns a sentinel the
// assertions match against, so we verify app.ts passes the agent preset through.
const { createTerminalMock, presetAgentTabbedMock } = vi.hoisted(() => ({
  createTerminalMock: vi.fn(),
  presetAgentTabbedMock: vi.fn(() => ["preset-features"]),
}));
vi.mock("@cplieger/web-terminal-ui", () => ({ createTerminal: createTerminalMock }));
vi.mock("@cplieger/web-terminal-ui/presets", () => ({
  presetAgentTabbed: presetAgentTabbedMock,
}));

// web-terminal-kiro's purple theme, passed through createTerminal (matches app.ts).
const THEME = {
  "--accent": "hsl(263.1683 100% 80%)",
  "--tab-hover-bg": "hsl(263.1683 100% 80% / 16%)",
  "--tab-active-bg": "hsl(263.1683 100% 80% / 32%)",
  "--tab-active-border": "color-mix(in oklch, var(--tab-active-bg), var(--text) 25%)",
  "--tab-active-fg": "#fff",
  "--status-working": "oklch(78% 0.15 300deg)",
  "--status-done": "oklch(78% 0.15 150deg)",
  "--status-input": "oklch(78% 0.15 95deg)",
};

// The fatal-overlay alertdialog contract duplicated (by necessity) between
// showFatal (app.ts) and the inline pre-module bootstrap watchdog
// (static/index.html). Both builders are asserted through this single helper
// so the two shapes cannot drift independently: a change to either side that
// breaks the shared shape fails here.
function expectFatalOverlayShape(overlay: HTMLElement): void {
  expect(overlay.getAttribute("role")).toBe("alertdialog");
  expect(overlay.getAttribute("aria-modal")).toBe("true");
  expect(overlay.getAttribute("aria-label")).toBe("Web Terminal for Kiro startup failure");
  expect(overlay.getAttribute("aria-describedby")).toBe("bootstrap-failure-message");
  // The pristine loading bar is always replaced by the dialog content.
  expect(overlay.querySelector(".bar")).toBeNull();
  const reload = overlay.querySelector("button");
  expect(reload?.type).toBe("button");
  expect(reload?.textContent).toBe("Reload");
  // Initial focus lands on the recovery CTA (the alertdialog pattern's
  // initial focus; Reload is the only actionable element left).
  expect(document.activeElement).toBe(reload);
}

describe("web-terminal-kiro bootstrap (app.ts)", () => {
  beforeEach(() => {
    // resetModules so each dynamic import re-runs app.ts top-level code. Mock
    // call history is cleared by the config's clearMocks/mockReset before each
    // test (implementations given to vi.fn persist through mockReset).
    vi.resetModules();
    document.body.replaceChildren();
  });

  it("throws a clear error when the #terminal root element is missing", async () => {
    await expect(import("./app.js")).rejects.toThrow(
      "web-terminal-kiro: missing #terminal root element",
    );
    expect(createTerminalMock).not.toHaveBeenCalled();
  });

  it("builds the terminal with the agent preset and theme when #loading is absent", async () => {
    const root = document.createElement("div");
    root.id = "terminal";
    document.body.appendChild(root);

    await import("./app.js");

    expect(createTerminalMock).toHaveBeenCalledTimes(1);
    expect(createTerminalMock).toHaveBeenCalledWith(root, {
      features: ["preset-features"],
      theme: THEME,
    });
  });

  it("passes the #loading element to createTerminal when it is present", async () => {
    const root = document.createElement("div");
    root.id = "terminal";
    document.body.appendChild(root);
    const loading = document.createElement("div");
    loading.id = "loading";
    document.body.appendChild(loading);

    await import("./app.js");

    expect(createTerminalMock).toHaveBeenCalledTimes(1);
    expect(createTerminalMock).toHaveBeenCalledWith(root, {
      features: ["preset-features"],
      theme: THEME,
      loading,
    });
  });

  it("surfaces an alert dialog on the #loading overlay when #terminal is missing but #loading exists", async () => {
    const overlay = document.createElement("div");
    overlay.id = "loading";
    overlay.setAttribute("role", "status"); // mirror index.html's static markup
    overlay.setAttribute("aria-label", "Loading");
    const bar = document.createElement("div");
    bar.className = "bar";
    overlay.appendChild(bar);
    document.body.appendChild(overlay);

    await expect(import("./app.js")).rejects.toThrow(
      "web-terminal-kiro: missing #terminal root element",
    );

    // The index.html watchdog only acts while the pristine .bar is present;
    // showFatal replacing the children (asserted inside the shape helper) is
    // what stops it from clobbering this message when the rethrown error
    // reaches the window error listener.
    expectFatalOverlayShape(overlay);
    const description = overlay.querySelector("#bootstrap-failure-message");
    expect(description?.textContent).toContain("Web Terminal for Kiro failed to start");
    expect(createTerminalMock).not.toHaveBeenCalled();
  });

  it("offers a working reload action when startup fails", async () => {
    const reload = vi.spyOn(window.location, "reload").mockImplementation(() => undefined);
    const overlay = document.createElement("div");
    overlay.id = "loading";
    document.body.appendChild(overlay);

    await expect(import("./app.js")).rejects.toThrow(
      "web-terminal-kiro: missing #terminal root element",
    );

    const reloadButton = overlay.querySelector("button");
    expectFatalOverlayShape(overlay);
    reloadButton?.click();
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("reveals the #loading overlay with an error message and rethrows when createTerminal throws", async () => {
    const root = document.createElement("div");
    root.id = "terminal";
    document.body.appendChild(root);
    const loading = document.createElement("div");
    loading.id = "loading";
    loading.classList.add("fade");
    loading.setAttribute("role", "status"); // mirror index.html's static markup
    loading.setAttribute("aria-label", "Loading");
    const bar = document.createElement("div");
    bar.className = "bar";
    loading.appendChild(bar);
    document.body.appendChild(loading);
    createTerminalMock.mockImplementationOnce(() => {
      throw new Error("kernel boom");
    });

    await expect(import("./app.js")).rejects.toThrow("kernel boom");

    expect(loading.classList.contains("fade")).toBe(false);
    expectFatalOverlayShape(loading);
    const description = loading.querySelector("#bootstrap-failure-message");
    expect(description?.textContent).toContain("Failed to start the terminal");
  });

  it("rethrows the original error without touching the DOM when createTerminal throws and #loading is absent", async () => {
    const root = document.createElement("div");
    root.id = "terminal";
    document.body.appendChild(root);
    createTerminalMock.mockImplementationOnce(() => {
      throw new Error("kernel boom no overlay");
    });

    await expect(import("./app.js")).rejects.toThrow("kernel boom no overlay");
    expect(createTerminalMock).toHaveBeenCalledTimes(1);
    expect(document.querySelector('[role="alertdialog"]')).toBeNull();
  });

  it("keeps the index.html bootstrap watchdog in sync with showFatal's alertdialog shape", () => {
    // Resolve from the vitest root (static-src/): under happy-dom
    // import.meta.url is not a file: URL, so a URL-relative read cannot work.
    const html = readFileSync(resolve(process.cwd(), "../static/index.html"), "utf8");
    // The failure-dialog vocabulary duplicated (by necessity) between showFatal
    // (app.ts) and the inline pre-module watchdog (static/index.html).
    expect(html).toContain('"alertdialog"');
    expect(html).toContain('"aria-modal", "true"');
    expect(html).toContain('"Web Terminal for Kiro startup failure"');
    expect(html).toContain('description.id = "bootstrap-failure-message"');
    expect(html).toContain('reload.textContent = "Reload"');
    expect(html).toContain("reload.focus()");
  });

  it("builds the same alertdialog shape when the real index.html watchdog fires", () => {
    // Execute the REAL inline bootstrap watchdog from static/index.html (the
    // pre-module, CSP-hashed script that catches /app.js load failures before
    // app.ts can run) against index.html's pristine pre-JS markup, and assert
    // it produces the exact overlay shape showFatal builds — via the same
    // expectFatalOverlayShape helper, so the two builders (which cannot share
    // code) are pinned to one contract from a single source. Mirrors how
    // routes_test.go independently re-extracts the same inline scripts for
    // the CSP hash check.
    const html = readFileSync(resolve(process.cwd(), "../static/index.html"), "utf8");
    // The watchdog is the only inline <script> that is neither the importmap
    // nor the src-bearing module loader.
    const scripts = [...html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script\s*>/gi)].filter(
      (m) => !/src\s*=/i.test(m[1] ?? "") && !/importmap/i.test(m[1] ?? ""),
    );
    expect(scripts).toHaveLength(1);
    const watchdogSource = scripts[0]?.[2] ?? "";
    expect(watchdogSource).toContain("Bootstrap watchdog");

    // Recreate index.html's static body: the terminal root plus the pristine
    // loading overlay (role=status, .bar child, no fade).
    const root = document.createElement("div");
    root.id = "terminal";
    document.body.appendChild(root);
    const overlay = document.createElement("div");
    overlay.id = "loading";
    overlay.setAttribute("role", "status");
    overlay.setAttribute("aria-label", "Loading");
    const bar = document.createElement("div");
    bar.className = "bar";
    overlay.appendChild(bar);
    document.body.appendChild(overlay);

    // Evaluate the watchdog, then simulate the failure it exists for: a
    // <script> element (e.g. /app.js) firing a load error on window.
    new Function(watchdogSource)();
    const scriptEl = document.createElement("script");
    const errorEvent = new Event("error");
    Object.defineProperty(errorEvent, "target", { value: scriptEl });
    window.dispatchEvent(errorEvent);

    expectFatalOverlayShape(overlay);
    const description = overlay.querySelector("#bootstrap-failure-message");
    expect(description?.textContent).toContain("Web Terminal for Kiro failed to load");
    // aria-modal made true: the watchdog inerts the terminal root, exactly
    // like showFatal.
    expect(root.hasAttribute("inert")).toBe(true);
  });
});
