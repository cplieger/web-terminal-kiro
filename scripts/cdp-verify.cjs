// CDP live-verify harness for the terminal rebuild (vterm/docs/REBUILD.md).
// Opens vibecli-dev in the shared Chromium sidecar, captures console
// errors/exceptions, lets kiro-cli render, and dumps a DOM/scroll snapshot so a
// brick can be checked against real kiro-cli without a real device.
//
// Zero deps (Node 22 global WebSocket + fetch). Cleans up the tab it opens.
//
// CRITICAL: the sidecar tab is opened in the background, where Chromium pauses
// requestAnimationFrame, so the rAF-batched renderer paints nothing. We force
// the page visible/focused via Page.bringToFront + Emulation.setFocusEmulation
// Enabled; without that the DOM stays empty even though the server is flushing
// frames correctly. (This bit us during brick 1 verification.)
//
// Usage: node scripts/cdp-verify.cjs [waitMs]
const CDP = process.env.CDP_URL || "http://192.168.1.77:9222";
const URL = process.env.VIBECLI_URL || "http://192.168.1.77:9849/";
const WAIT = Number(process.argv[2] || 13000);

function rpc(ws, id, method, params) {
  return new Promise((resolve, reject) => {
    const onMsg = (ev) => {
      let m;
      try { m = JSON.parse(ev.data); } catch { return; }
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
  const target = await fetch(`${CDP}/json/new?${encodeURIComponent(URL)}`, { method: "PUT" }).then((r) => r.json());
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.addEventListener("open", res); ws.addEventListener("error", rej); });
  let id = 0;
  const errors = [];
  ws.addEventListener("message", (ev) => {
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    if (m.method === "Runtime.consoleAPICalled" && (m.params.type === "error" || m.params.type === "warning")) {
      errors.push(m.params.type + ": " + m.params.args.map((a) => a.value ?? a.description ?? "").join(" "));
    }
    if (m.method === "Runtime.exceptionThrown") {
      errors.push("EXC: " + (m.params.exceptionDetails.exception?.description ?? m.params.exceptionDetails.text));
    }
  });
  await rpc(ws, ++id, "Runtime.enable", {});
  await rpc(ws, ++id, "Page.enable", {});
  await rpc(ws, ++id, "Page.bringToFront", {}).catch(() => {});
  await rpc(ws, ++id, "Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {});

  await sleep(WAIT);

  const expr = `(async () => {
    const rafFired = await new Promise((res) => { let f=false; requestAnimationFrame(()=>{f=true;}); setTimeout(()=>res(f),250); });
    const out = document.getElementById('term-output');
    const term = document.getElementById('term');
    const input = document.getElementById('term-input');
    const rows = out ? Array.from(out.children) : [];
    const text = rows.map(r => (r.textContent||'').replace(/\\u00a0/g,' ').replace(/\\s+$/,'')).filter(t => t.length);
    let maxRunDup=1,cur=1; for(let i=1;i<text.length;i++){ if(text[i]===text[i-1]&&text[i]!==''){cur++;maxRunDup=Math.max(maxRunDup,cur);}else cur=1; }
    return JSON.stringify({
      visibilityState: document.visibilityState, hasFocus: document.hasFocus(), rafFired,
      appRan: input ? (input.value.charCodeAt(0) === 0xa0) : 'no-input-el',
      rowCount: rows.length, nonEmptyLines: text.length,
      firstLines: text.slice(0,3), lastLines: text.slice(-4),
      maxConsecutiveDup: maxRunDup,
      termClientH: term?.clientHeight, termScrollH: term?.scrollHeight, termScrollTop: term?.scrollTop,
      loadingPresent: !!document.getElementById('loading'), docTitle: document.title
    }, null, 2);
  })()`;
  const r = await rpc(ws, ++id, "Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
  console.log("=== console errors/warnings ===");
  console.log(errors.length ? errors.slice(0, 20).join("\n") : "(none)");
  console.log("=== DOM snapshot ===");
  console.log(r.result.value);

  ws.close();
  await fetch(`${CDP}/json/close/${target.id}`);
  process.exit(0);
})().catch((e) => { console.error("VERIFY ERROR:", e.message); process.exit(1); });
