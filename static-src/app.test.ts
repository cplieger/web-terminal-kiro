// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from "vitest";

const { mountMock } = vi.hoisted(() => ({ mountMock: vi.fn() }));
vi.mock("@cplieger/web-terminal-ui", () => ({ mount: mountMock }));

describe("vibecli bootstrap (app.ts)", () => {
  beforeEach(() => {
    // resetModules so each dynamic import re-runs app.ts top-level code;
    // clearMocks (vitest.config) clears mountMock call history between tests.
    vi.resetModules();
    document.body.replaceChildren();
  });

  it("throws a clear error when the #terminal root element is missing", async () => {
    await expect(import("./app.js")).rejects.toThrow("vibecli: missing #terminal root element");
    expect(mountMock).not.toHaveBeenCalled();
  });

  it("mounts into #terminal with empty options when #loading is absent", async () => {
    const root = document.createElement("div");
    root.id = "terminal";
    document.body.appendChild(root);

    await import("./app.js");

    expect(mountMock).toHaveBeenCalledTimes(1);
    expect(mountMock).toHaveBeenCalledWith(root, {});
  });

  it("passes the #loading element to mount when it is present", async () => {
    const root = document.createElement("div");
    root.id = "terminal";
    document.body.appendChild(root);
    const loading = document.createElement("div");
    loading.id = "loading";
    document.body.appendChild(loading);

    await import("./app.js");

    expect(mountMock).toHaveBeenCalledTimes(1);
    expect(mountMock).toHaveBeenCalledWith(root, { loading });
  });

  it("surfaces an alert on the #loading overlay when #terminal is missing but #loading exists", async () => {
    const overlay = document.createElement("div");
    overlay.id = "loading";
    document.body.appendChild(overlay);

    await expect(import("./app.js")).rejects.toThrow("vibecli: missing #terminal root element");

    expect(overlay.getAttribute("role")).toBe("alert");
    expect(overlay.textContent).toContain("vibecli failed to start");
    expect(mountMock).not.toHaveBeenCalled();
  });

  it("reveals the #loading overlay with an error message and rethrows when mount throws", async () => {
    const root = document.createElement("div");
    root.id = "terminal";
    document.body.appendChild(root);
    const loading = document.createElement("div");
    loading.id = "loading";
    loading.classList.add("fade");
    document.body.appendChild(loading);
    mountMock.mockImplementationOnce(() => {
      throw new Error("mount boom");
    });

    await expect(import("./app.js")).rejects.toThrow("mount boom");

    expect(loading.classList.contains("fade")).toBe(false);
    expect(loading.getAttribute("role")).toBe("alert");
    expect(loading.textContent).toContain("Failed to start the terminal");
  });

  it("rethrows the original error without touching the DOM when mount throws and #loading is absent", async () => {
    const root = document.createElement("div");
    root.id = "terminal";
    document.body.appendChild(root);
    mountMock.mockImplementationOnce(() => {
      throw new Error("mount boom no overlay");
    });

    await expect(import("./app.js")).rejects.toThrow("mount boom no overlay");
    expect(mountMock).toHaveBeenCalledTimes(1);
  });
});
