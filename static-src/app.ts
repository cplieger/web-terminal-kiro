// vibecli client entry point.
//
// All terminal behavior lives in the shared packages: the
// @cplieger/web-terminal-engine engine (render / scroll / connection / keyboard)
// and the @cplieger/web-terminal-ui reference UI (the textarea input model,
// mobile key toolbar, context menu, IME, predictive echo, viewport handling).
// vibecli is the thinnest possible consumer: mount() builds the whole terminal
// UI inside the #terminal root element; kiro-cli's TUI is driven verbatim
// through the raw PTY stream.
//
// The server serves the WebSocket at "/ws" (mount's default), and the bundled
// font is Monaspace (the fontReady default), so the only option passed is the
// loading overlay mount fades out once the first frame renders.

import { mount } from "@cplieger/web-terminal-ui";

const root = document.getElementById("terminal");
if (!root) {
  throw new Error("vibecli: missing #terminal root element");
}
const loading = document.getElementById("loading");
mount(root, loading ? { loading } : {});
