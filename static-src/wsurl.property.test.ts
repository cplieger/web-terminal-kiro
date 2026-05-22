// Property-based test for wsURL — the only currently-pure helper exposed
// from app.ts. Acts as the bootstrap test for vibecli's vitest+fast-check
// pipeline; demonstrates the intended convention for future tests.
//
// Invariants tested:
// 1. The `wss://` scheme is selected exactly when the page protocol is
//    "https:"; for any other protocol value, `ws://` is selected.
// 2. The host is preserved verbatim in the result (no encoding,
//    truncation, or rewriting).
// 3. The path suffix `/ws` is always present.
// 4. The result is a parseable URL.

import { describe, it, expect } from "vitest";
import fc from "fast-check";

import { wsURL } from "./urls.js";

describe("wsURL property", () => {
  it("selects wss: only when page protocol is https:, ws: otherwise", () => {
    fc.assert(
      fc.property(
        fc.constantFrom("http:", "https:", "file:", "ftp:", ""),
        fc.string({ minLength: 1, maxLength: 100 }),
        (proto, host) => {
          const url = wsURL(proto, host);
          if (proto === "https:") {
            expect(url.startsWith("wss://")).toBe(true);
          } else {
            expect(url.startsWith("ws://")).toBe(true);
          }
        },
      ),
    );
  });

  it("preserves the host verbatim in the URL", () => {
    fc.assert(
      fc.property(
        fc.constantFrom("http:", "https:"),
        fc.domain(),
        (proto, host) => {
          const url = wsURL(proto, host);
          expect(url).toContain(host);
        },
      ),
    );
  });

  it("always ends with /ws", () => {
    fc.assert(
      fc.property(
        fc.constantFrom("http:", "https:"),
        fc.domain(),
        (proto, host) => {
          const url = wsURL(proto, host);
          expect(url.endsWith("/ws")).toBe(true);
        },
      ),
    );
  });

  it("produces a parseable URL with the expected scheme and path", () => {
    fc.assert(
      fc.property(
        fc.constantFrom("http:", "https:"),
        fc.domain(),
        (proto, host) => {
          const url = wsURL(proto, host);
          // URL parsing accepts ws:// and wss:// (registered schemes).
          const parsed = new URL(url);
          expect(parsed.protocol).toBe(proto === "https:" ? "wss:" : "ws:");
          expect(parsed.host).toBe(host);
          expect(parsed.pathname).toBe("/ws");
        },
      ),
    );
  });
});
