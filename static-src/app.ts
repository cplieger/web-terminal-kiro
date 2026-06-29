// vibecli client entry point.
//
// All terminal behavior now lives in the shared packages: the
// @cplieger/web-terminal engine (render / scroll / connection / keyboard) and
// the @cplieger/web-terminal-ui reference UI (the textarea input model, mobile
// key toolbar, context menu, IME, predictive echo, viewport handling). vibecli
// is a thin kiro-cli integration, so its client is a single mount() call
// against the default scaffold; kiro-cli's TUI is driven verbatim through the
// terminal stream.
//
// The server serves the WebSocket at "/ws" (mount's default), and the bundled
// font is Monaspace (the fontReady default), so no options are needed.

import { mount } from "@cplieger/web-terminal-ui";

mount();
