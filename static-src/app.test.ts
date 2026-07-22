// @vitest-environment happy-dom
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

    expect(overlay.getAttribute("role")).toBe("alertdialog");
    // The index.html watchdog only acts while the pristine .bar is present;
    // showFatal replacing the children is what stops it from clobbering this
    // message when the rethrown error reaches the window error listener.
    expect(overlay.querySelector(".bar")).toBeNull();
    expect(overlay.getAttribute("aria-modal")).toBe("true");
    expect(overlay.getAttribute("aria-label")).toBe("Web Terminal for Kiro startup failure");
    expect(overlay.getAttribute("aria-describedby")).toBe("bootstrap-failure-message");
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
    expect(reloadButton?.type).toBe("button");
    expect(reloadButton?.textContent).toBe("Reload");
    // showFatal moves focus to the recovery CTA: the page content is gone and
    // Reload is the only actionable element left.
    expect(document.activeElement).toBe(reloadButton);
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
    expect(loading.getAttribute("role")).toBe("alertdialog");
    expect(loading.querySelector(".bar")).toBeNull();
    expect(loading.getAttribute("aria-modal")).toBe("true");
    expect(loading.getAttribute("aria-label")).toBe("Web Terminal for Kiro startup failure");
    expect(loading.getAttribute("aria-describedby")).toBe("bootstrap-failure-message");
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
});
