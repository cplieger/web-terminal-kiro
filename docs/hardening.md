# Security

This page covers what an exposed Web Terminal for Kiro gives away, how to put it behind a reverse proxy, and what the image contains, for anyone opening it beyond their own machine.

## What a tab can reach

A browser tab is an interactive root shell. It reads and writes your files under `/workspace`, and it can read kiro-cli's stored sign-in plus the SSH keys and git config under `/config/home`. Anyone who can reach the port can use it, and Web Terminal for Kiro has no login of its own. Everyone who opens it shares one kiro-cli sign-in and one set of files. Before you expose it beyond your own machine, do at least one of these, and ideally both:

- Put it behind a reverse proxy that asks for a login, such as Caddy forward-auth, oauth2-proxy or Authentik.
- Keep the published port on loopback or a private network.

The server logs a warning at startup when it listens on an address beyond loopback, and another when `ALLOWED_HOSTS` is unset.

## DNS rebinding and ALLOWED_HOSTS

Neither step above stops DNS rebinding. A malicious web page open in your own browser can point its own hostname at `127.0.0.1`, or at your LAN address, and drive even a terminal that listens on loopback only. The request then comes from your own machine, and its `Origin` matches, so the origin check admits it.

Set [`ALLOWED_HOSTS`](configuration.md#allowed_hosts) to the exact names you open the terminal at. The host list is the check that refuses a rebound request. Set it for any setup that runs for longer than a test.

## Behind a reverse proxy

Terminate TLS at the proxy and require a login there: HTTP Basic auth at minimum, or forward auth with Authentik, oauth2-proxy or Caddy forward-auth for single sign-on. [Running an app behind a reverse proxy](https://github.com/cplieger/docs/blob/main/docs/reverse-proxy.md) has complete Caddy, nginx, Traefik and Nginx Proxy Manager setups for the WebSocket and the headers. Its examples add no login, so add one as below.

This Caddy site asks for a password and passes WebSocket connections through on its own:

```caddyfile
webterm.example.com {
	basic_auth {
		# Your user name, then the output of: caddy hash-password
		alice <password hash>
	}
	reverse_proxy 192.168.1.5:9848
}
```

Replace `192.168.1.5` with the address of the Docker host, and put `webterm.example.com` in `ALLOWED_HOSTS`.

Set [`TRUSTED_PROXIES`](configuration.md#trusted_proxies) to the proxy's address, as [Telling the app about the proxy](https://github.com/cplieger/docs/blob/main/docs/reverse-proxy.md#telling-the-app-about-the-proxy) explains.

### The session id in the proxy log

A terminal's WebSocket address carries the session id as a query parameter, `/ws?session=<id>`, and that id works like a key. Anyone who can reach the port and send it attaches to that live session. Sessions have no idle timeout, so the id stays valid until the tab is closed or the container restarts. This server never logs it.

A proxy usually logs the full request address by default, such as Caddy's `uri` field or nginx's `$request`. Drop or redact the query string for `/ws` in the proxy's access log before you ship that log anywhere.

## Pasted images

The page posts a pasted image to `/api/uploads`. That address passes the same host check and cross-origin check as the terminal, and it has no login either. It grants nothing a tab does not already have, since a tab is a root shell. The server writes only to `/uploads`, under a name that must match the `pasted-<date>T<time>` pattern, and refuses a request larger than 256 MiB.

## Stored scrollback

Each tab's newest 200 lines are kept in your browser's `localStorage`. A phone that discarded the page then asks the server only for what it missed, instead of pulling every tab's history again. Terminal output is not always something you want on disk, so here is what that keeps:

- The lines can be read from that browser without reaching this server, and they outlive the tab. An entry is deleted when you close its terminal, and otherwise after seven days.
- Nothing is sent anywhere. The server neither receives nor reads these copies.
- Lines from a previous run of the container are cleared, so a restart never leaves the last run's history on screen.
- On a shared or borrowed device, use a private window and close every private tab when you finish. The browser decides whether a private window's storage is unavailable or only temporary.

No setting turns this off.

## Root, and files on the host

The image runs as root so `git`, `gh` and SSH work, and it finds your `~/.ssh` under `/config/home`. Do not add a `user:` line. Files the container writes to `/config` and `/workspace` belong to root on the host.

## What the image contains

| Dependency | Source |
| --- | --- |
| Debian trixie-slim | Base image, pinned by digest. `apt-get upgrade` runs at build time. |
| `kiro-cli` | Downloaded at first start and checked against a pinned digest, never baked into the image, for licensing reasons. |
| `web-terminal-engine`, `@cplieger/web-terminal-ui` | The terminal engine and the browser page. |
| Monaspace Neon NF | The terminal's web font, fetched at build time and checked against a digest per face. |
| `web-terminal-glyphs` | The overlay font for box drawing, blocks, shades, braille and mosaics, digest-checked at build time. Its cell sizes gate the build against the served CSS. |
| `toolbelt`, `tool-catalog` | The tools engine and the catalog it installs from. |
| `pinstall` | The installer that downloads, verifies and activates the pinned kiro-cli. |
| `webhttp`, `envx`, `slogx`, `atomicfile` | HTTP handling, settings, logging and safe file writes. |

Every version is pinned, and every download at build time is checked against a recorded sha256. Updates arrive as automated pull requests and ship in a new image build.
