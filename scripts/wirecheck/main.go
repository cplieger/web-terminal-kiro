// Command wirecheck asserts wire-protocol compatibility between the Go
// server half (the web-terminal-engine module go.mod pins) and the served
// TS client half (the Dockerfile-ARG-pinned npm artifact), using the
// engine's exported compatibility floors. The Dockerfile's wire-floor gate
// extracts the client's constants from the vendored artifact and passes
// them as flags; this program supplies the Go side from the engine's public
// API — no source scraping on the Go half.
//
// Exit 0: the pairing is declared-compatible. Exit 1: a declared floor is
// violated — the pairing would refuse at first connect with close code 4002,
// so fail the build instead. A MIS-declared floor is out of scope here; that
// is the engine conformance suite's contract.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cplieger/web-terminal-engine/v3/terminal"
)

func main() {
	clientRev := flag.Int("client-rev", 0, "client WIRE_PROTOCOL_VERSION from the vendored npm artifact")
	clientMinServer := flag.Int("client-min-server", 0, "client MIN_SUPPORTED_SERVER_WIRE_VERSION from the vendored npm artifact")
	flag.Parse()
	if *clientRev <= 0 || *clientMinServer <= 0 {
		fmt.Fprintln(os.Stderr, "wirecheck: -client-rev and -client-min-server are required positive integers")
		os.Exit(2)
	}
	if reason := incompatibility(terminal.WireProtocolVersion, terminal.MinSupportedClientWireVersion, *clientRev, *clientMinServer); reason != "" {
		fmt.Fprintf(os.Stderr, "ERROR wire-floor-mismatch: %s\n", reason)
		os.Exit(1)
	}
	fmt.Printf("wirecheck ok: server wire rev %d (min client %d) <-> client wire rev %d (min server %d)\n",
		terminal.WireProtocolVersion, terminal.MinSupportedClientWireVersion, *clientRev, *clientMinServer)
}

// incompatibility returns "" when the declared floors admit the pairing in
// both directions, or a human-readable reason naming the violated floor and
// which pin to move.
func incompatibility(serverRev, serverMinClient, clientRev, clientMinServer int) string {
	if serverRev < clientMinServer {
		return fmt.Sprintf(
			"Go engine wire revision %d is below the npm client's minimum supported server revision %d (bump go.mod's web-terminal-engine)",
			serverRev, clientMinServer)
	}
	if clientRev < serverMinClient {
		return fmt.Sprintf(
			"npm client wire revision %d is below the Go engine's minimum supported client revision %d (bump the Dockerfile ARG + static-src/package.json engine pins)",
			clientRev, serverMinClient)
	}
	return ""
}
