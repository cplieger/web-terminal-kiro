// Pure URL helpers, no DOM/terminal/WebSocket dependencies.
// Kept in a dedicated module so unit tests can import it without
// pulling in xterm.js (which is loaded as a browser-side vendor and
// is not installed via npm at test time).

// wsURL builds the WebSocket URL from a page protocol/host pair.
// Pulled out and parameterized so it can be property-tested without
// touching `location`. The caller threads the live `location.*` through.
export function wsURL(pageProtocol: string, pageHost: string): string {
  const wsProto = pageProtocol === "https:" ? "wss:" : "ws:";
  return `${wsProto}//${pageHost}/ws`;
}
