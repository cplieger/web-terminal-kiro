// @vitest-environment happy-dom

// Regression test for the permission-prompt auto-follow bug: a
// programmatic scrollToBottom() fires its own 'scroll' event, which must
// NOT be read as a user scroll (doing so re-armed the user-scroll window
// / userScrolledUp and latched auto-follow off after the first arrow).
// A real user scroll must still register, and the suppression is one-shot
// so it can't swallow genuine scrolls during streaming.

import { describe, it, expect } from "vitest";
import * as scroll from "./scroll.js";

function makeScrollEl(): HTMLElement {
  const el = document.createElement("div");
  let top = 600; // scrollHeight(1000) - clientHeight(400) => at bottom
  Object.defineProperty(el, "scrollTop", {
    get: () => top,
    set: (v: number) => {
      top = v;
    },
    configurable: true,
  });
  Object.defineProperty(el, "scrollHeight", { get: () => 1000, configurable: true });
  Object.defineProperty(el, "clientHeight", { get: () => 400, configurable: true });
  return el;
}

describe("scroll: programmatic re-pin vs user scroll", () => {
  it("ignores its own scroll event but still detects real user scrolls", () => {
    const el = makeScrollEl();
    scroll.init({ scrollEl: el });

    // Real user scroll up is detected.
    el.scrollTop = 0;
    el.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(true);
    expect(scroll.isInUserScroll()).toBe(true);

    // Programmatic re-pin: its self-induced 'scroll' event must not
    // re-arm the user-scroll window (this is what disengaged auto-follow).
    scroll.scrollToBottom();
    el.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(false);
    expect(scroll.isInUserScroll()).toBe(false);

    // One-shot: the next genuine user scroll is still registered.
    el.scrollTop = 0;
    el.dispatchEvent(new Event("scroll"));
    expect(scroll.isUserScrolledUp()).toBe(true);
  });
});
