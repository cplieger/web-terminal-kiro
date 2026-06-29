// CDP live-verify for brick 5 (input / keyboard / selection separation),
// vterm/docs/REBUILD.md — bugs 1, 6, 7. Checks the structural facts that make
// those bugs go away, the parts a desktop sidecar CAN observe:
//
//   - #term-output is NOT contenteditable (the editable overload that made the
//     first touch-drag a caret gesture (bug 6) and collapsed selections (bug 1))
//   - the textarea (#term-input), not the output, is the focused element after
//     load — the unified focus model
//   - #term has native overflow-anchor: auto (the read position holds itself)
//   - the context menu, opened hard against the bottom-right corner, is clamped
//     fully inside the viewport (bug 1's off-screen callout)
//   - a selection made over a committed row SURVIVES several streaming frames
//     (the core of bug 1 — selection is no longer destroyed every redraw)
//
// What it CANNOT check (needs a real iOS device on WebKit, flagged in the doc):
//   - that the first touch-drag actually scrolls (bug 6)
//   - that a tap on a sparse screen summons the keyboard (bug 7)
//   - native long-press selection callout behavior
//
// Run against the :9850 emitter (continuous output, so the selection-survival
// check has frames to survive). Zero deps (Node 22 global WebSocket + fetch).
//
// Usage: node scripts/cdp-bug5.cjs
const CDP = process.env.CDP_URL || "http://192.168.1.77:9222";
const URL = process.env.VIBECLI_URL || "http://192.168.1.77:9850/";
const SETTLE_MS = Number(process.env.SETTLE_MS || 10000);
const SELECT_HOLD_MS = Number(process.env.SELECT_HOLD_MS || 4000);

function rpc(ws, id, method, params) {
  return new Promise((resolve, reject) => {
    const onMsg = (ev) => {
      let m;
      try {
        m = JSON.parse(ev.data);
      } catch {
        return;
      }
      if (m.id === id) {
        ws.removeEventListener("message", onMsg);
        m.error ? reject(new Error(method + ": " + JSON.stringify(m.error))) : resolve(m.result);
      }
    };
    ws.addEventListener("message", onMsg);
    ws.send(JSON.stringify({ id, method, params }));
  });
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  const target = await fetch(`${CDP}/json/new?${encodeURIComponent(URL)}`, { method: "PUT" }).then(
    (r) => r.json(),
  );
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => {
    ws.addEventListener("open", res);
    ws.addEventListener("error", rej);
  });
  let id = 0;
  const errors = [];
  ws.addEventListener("message", (ev) => {
    let m;
    try {
      m = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (m.method === "Runtime.consoleAPICalled" && m.params.type === "error") {
      errors.push("error: " + m.params.args.map((a) => a.value ?? a.description ?? "").join(" "));
    }
    if (m.method === "Runtime.exceptionThrown") {
      errors.push(
        "EXC: " +
          (m.params.exceptionDetails.exception?.description ?? m.params.exceptionDetails.text),
      );
    }
  });
  await rpc(ws, ++id, "Runtime.enable", {});
  await rpc(ws, ++id, "Page.enable", {});
  await rpc(ws, ++id, "Page.bringToFront", {}).catch(() => {});
  await rpc(ws, ++id, "Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {});

  await sleep(SETTLE_MS);

  const evaluate = async (expression, awaitPromise = false) => {
    const r = await rpc(ws, ++id, "Runtime.evaluate", {
      expression,
      returnByValue: true,
      awaitPromise,
    });
    return r.result.value;
  };

  // --- structural facts + the context-menu clamp (one synchronous probe) ---
  const structural = JSON.parse(
    await evaluate(`(() => {
      const out = document.getElementById('term-output');
      const term = document.getElementById('term');
      const input = document.getElementById('term-input');
      const active = document.activeElement;
      // Open the context menu hard against the bottom-right corner.
      term.dispatchEvent(new MouseEvent('contextmenu', {
        bubbles: true, cancelable: true,
        clientX: window.innerWidth - 2, clientY: window.innerHeight - 2,
      }));
      const menu = document.getElementById('ctx-menu');
      const r = menu.getBoundingClientRect();
      const clampedIn = r.left >= 0 && r.top >= 0 &&
        r.right <= window.innerWidth && r.bottom <= window.innerHeight && r.width > 0;
      menu.classList.remove('visible'); // tidy up
      return JSON.stringify({
        outputContentEditable: out.isContentEditable,
        activeIsTextarea: active === input,
        activeId: active ? active.id : null,
        overflowAnchor: getComputedStyle(term).overflowAnchor,
        outputUserSelect: getComputedStyle(out).userSelect,
        menuRect: { left: Math.round(r.left), top: Math.round(r.top), right: Math.round(r.right), bottom: Math.round(r.bottom) },
        menuClampedInViewport: clampedIn,
        viewport: { w: window.innerWidth, h: window.innerHeight },
      });
    })()`),
  );

  // --- selection survival across streaming frames ---
  const selBefore = await evaluate(`(() => {
    const out = document.getElementById('term-output');
    const rows = Array.from(out.children).filter(r => (r.textContent||'').trim().length > 5);
    if (rows.length < 3) return '';
    const row = rows[1]; // an early (committed/frozen) row
    const range = document.createRange();
    range.selectNodeContents(row);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    return sel.toString();
  })()`);
  await sleep(SELECT_HOLD_MS); // emitter keeps printing; frames flush
  const selAfter = await evaluate(`window.getSelection().toString()`);

  console.log("=== console errors ===");
  console.log(errors.length ? errors.slice(0, 20).join("\n") : "(none)");
  console.log("=== structural ===");
  console.log(JSON.stringify(structural, null, 2));
  console.log("=== selection survival ===");
  console.log(`before(${selBefore.length} chars): ${JSON.stringify(selBefore.slice(0, 60))}`);
  console.log(`after (${selAfter.length} chars): ${JSON.stringify(selAfter.slice(0, 60))}`);

  const checks = {
    "no console errors": errors.length === 0,
    "#term-output is NOT contenteditable": structural.outputContentEditable === false,
    "textarea is the focused element (not the output)": structural.activeIsTextarea === true,
    "#term has native overflow-anchor: auto": structural.overflowAnchor === "auto",
    "#term-output keeps user-select: text": structural.outputUserSelect === "text",
    "context menu clamped inside the viewport": structural.menuClampedInViewport === true,
    "a selection was made over a committed row": selBefore.length > 0,
    "selection survives streaming frames (bug 1 core)":
      selBefore.length > 0 && selBefore === selAfter,
  };
  console.log("=== verdict ===");
  let ok = true;
  for (const [k, v] of Object.entries(checks)) {
    console.log(`${v ? "PASS" : "FAIL"}  ${k}`);
    ok = ok && v;
  }
  console.log(ok ? "\nBRICK 5 STRUCTURAL VERIFY: PASS" : "\nBRICK 5 STRUCTURAL VERIFY: FAIL");

  ws.close();
  await fetch(`${CDP}/json/close/${target.id}`);
  process.exit(ok ? 0 : 1);
})().catch((e) => {
  console.error("VERIFY ERROR:", e.message);
  process.exit(2);
});
