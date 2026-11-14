# Configuration

This page lists every setting of Web Terminal for Kiro and what each one changes, for readers who want more than the quick start sets.

## Where settings live

Settings are environment variables on the container, under `environment:` in `compose.yaml`. The server reads them once at start, so recreate the container after a change. No setting is required, and the image ships working defaults. A malformed value logs a warning that names the variable, never the value, and the server falls back to the default. `ALLOWED_HOSTS` is the exception, described below.

The image also ships three kiro-cli defaults in `/etc/kiro-cli/default-settings.json`. They turn off telemetry and the warning that kiro-cli runs as root, since every tab runs as root by design. They also turn off copy-on-select in full-screen mode, which reports "Copied" in this image while copying nothing ([Kiro#12210](https://github.com/kirodotdev/Kiro/issues/12210)). Shift+drag still selects text in the browser.

Run `kiro-cli settings <key> <value>` in a tab to change one of these defaults, and your value stays after a restart. Four other kiro-cli settings are reset at every start, because the app relies on them: `app.disableAutoupdates`, `chat.enableNotifications`, `chat.notificationMethod` and `chat.terminalTitle`.

## Every setting

| Variable | Description | Default |
| --- | --- | --- |
| `ALLOWED_HOSTS` | Comma-separated hostnames and IPs you open the terminal at. Any other host name is refused. Unset accepts every host and logs a warning. | _(unset)_ |
| `TRUSTED_PROXIES` | Your reverse proxy's IPs or address ranges. The access log then takes the real client address from the proxy's `X-Forwarded-For` header. | _(unset)_ |
| `LISTEN_ADDR` | Listen address as `host:port`. The healthcheck asks the same address, or `127.0.0.1` when the host part is empty. | `:9848` |
| `KIRO_CLI_CHAT_ARGS` | Extra flags added to every tab's `kiro-cli chat` command, separated by spaces, for example `--effort high`. | _(unset)_ |
| `KIRO_CLI_HIGH_THROUGHPUT` | Experimental. `true` tunes each tab's kiro-cli for speed when it runs many sessions at once, as workflows and subagents do. | `false` |
| `LOG_LEVEL` | `debug`, `info`, `warn` or `error`. An unknown value falls back to `info` with a warning. | `info` |
| `WORK_DIR` | The folder each new tab starts in. It must exist. | `/workspace` |
| `SCROLLBACK` | Lines of history kept per tab. kiro-cli clears history on each full-screen redraw, so a tab keeps at most about 3000 lines. | `100000` |
| `TOOL_CATALOG_REFRESH` | How often the tool catalog is refreshed, as a duration such as `24h`. `off` or `0` stops the schedule. | `24h` |
| `TOOL_CATALOG_URL` | Where catalog refreshes come from. Point it at a fork or a mirror of the catalog. | the [tool-catalog](https://github.com/cplieger/tool-catalog) latest release |
| `TOOL_CATALOG_PATH` | The catalog built into the image, used at first start and offline until a fetched catalog replaces it. | `/app/tool-catalog.json` |
| `BUNDLED_TOOLS_PATH` | The file in the image naming the four language servers no registry carries, merged over every loaded catalog. | `/app/bundled-tools.json` |
| `GH_TOKEN` | A GitHub token for tool version checks, which raises GitHub's limit of 60 requests an hour. Every tab sees it. Takes precedence over `GITHUB_TOKEN`. | _(unset)_ |
| `GITHUB_TOKEN` | Read in place of `GH_TOKEN` when that one is unset or empty. | _(unset)_ |
| `TRUSTED_INSTALL_UIDS` | Numeric user IDs trusted to write the folder holding kiro-cli and your tools. Leave it unset unless the startup check refuses a folder you trust. | _(unset)_ |
| `KIRO_CLI_SANDBOX` | Check at start that kiro-cli's command sandbox can run, and give it bubblewrap when it can. Needs the profiles in [Command sandbox](sandbox.md). | `false` |

## ALLOWED_HOSTS

List the exact names and addresses you type in the browser, for example `localhost,192.0.2.5,webterm.example.com`. Use bare names and addresses only, with no scheme, path or CIDR range such as `192.0.2.0/24`. This list is the check that stops DNS rebinding, described in [Security](hardening.md).

A request that is loopback on both ends is always admitted, so the healthcheck and commands run inside the container keep working. Both the client address and the `Host` must be loopback for that, as with `127.0.0.1:9848` or `localhost:9848`. When `LISTEN_ADDR` names one IP address, a request from that address to that address is admitted too, which is how the healthcheck reaches it. Any other name still needs to be in the list. An uptime prober from outside must use a name in the list.

Malformed entries are dropped with a warning that gives their count. When every entry is malformed, the server refuses every request that is not loopback and logs why.

## TRUSTED_PROXIES

With this unset, the access log records the address that opened the connection and ignores `X-Forwarded-For`, so nobody can fake the logged address. That is the right choice when the terminal is reached directly. Behind a proxy, set it to the proxy's addresses, for example `192.0.2.0/28,192.0.2.100`. A malformed entry is logged and skipped, and startup continues. A default route such as `0.0.0.0/0` logs a warning, because it treats every client as a proxy.

## LISTEN_ADDR

The image's healthcheck asks `/healthz` at the host and port this value names, or on `127.0.0.1` when the host part is empty, `0.0.0.0` or `::`. A host given as a name other than `localhost` must also be in `ALLOWED_HOSTS` when that is set, or the healthcheck is refused and reports the container `unhealthy`. Limit who can reach the port with the published port, for example `127.0.0.1:9848:9848`, or with a reverse proxy.

## KIRO_CLI_CHAT_ARGS

Use it for any flag `kiro-cli chat` accepts, for example `--effort high`, or `--v2` to keep the agent engine kiro-cli used before 3.0. The flags go only to `kiro-cli chat`, not to the sign-in commands. Their values never reach the log. The startup line records only how many flags there are. The flags also apply to every conversation restored after a restart, so a `--model` or `--agent` given here replaces the model or agent that conversation was using.

## KIRO_CLI_HIGH_THROUGHPUT

Off unless you set it. With it on, each tab's kiro-cli starts its agent engine with `--max-semi-space-size=64` and `--experimental-async-context-frame`. The flags go on the engine's own command line, through kiro-cli's `KIRO_KAS_NODE_PATH`, and never into `NODE_OPTIONS`. Every command the agent runs inherits the engine's environment, and Node.js 26 and later refuse to start with the second flag in `NODE_OPTIONS`. A `NODE_OPTIONS` you set yourself still reaches the engine unchanged.

The flags were measured on Node.js 22.23.4, the version kiro-cli 2.37.1 and 3.0.0 bundle. Before each tab starts, the server checks kiro-cli's unpacked Node.js against its `node.sha256`, against the hash the active kiro-cli embeds, and against that version. When any check fails, the tab starts as stock kiro-cli and the log carries one warning naming the reason, or both versions. A tab started before kiro-cli unpacked its Node.js also starts stock, and that start is what unpacks it. A `KIRO_KAS_NODE_PATH` already set on the container is left alone, with a warning, and the setting does nothing.

The gain depends on how many sessions one engine carries. The test drove a scratch engine with 32 concurrent sessions and a 4 GB heap. There the flags raised throughput from 2.6-3.0 to 10.1 prompts a second, cut time in garbage collection from 36-43% to 12%, and cut event-loop lag from 14-35 seconds to 4 seconds.

With one or two concurrent sessions, the load of a tab you type in, they cut the time in garbage collection from about 5% to 2.5% and gave no throughput gain. Turn it on for tabs that run workflows or many subagents. `--experimental-async-context-frame` is experimental in Node.js 22 and the default from Node.js 24.

## LOG_LEVEL

The value is read in any case, so `DEBUG` works too. `debug` adds the diagnostics behind each tab's name and the mark a workflow run puts on it, which help when one stays stuck.

## SCROLLBACK

History is held in memory and grows as a session produces it, so a large value costs nothing until a tab reaches it. kiro-cli clears its history on every full-screen repaint. A tab therefore keeps at most about 3000 lines, and raising it gains nothing. `0` keeps nothing beyond the live screen. A value from `1` to `2000` is raised to `2001` with a warning. Below that depth, the browser keeps its whole buffer instead. This variable has the same name and meaning in every app built on [web-terminal-engine](https://github.com/cplieger/web-terminal-engine).

## TOOL_CATALOG_REFRESH

With the schedule off, a refresh still runs when a command inside the container asks for one, as `curl -X POST localhost:9848/api/tools/catalog/refresh`. [Tools](tools.md) covers the catalog.

## GH_TOKEN

The tools engine asks GitHub's API for the latest version of each tool and for the files of a GitHub release. Without a token, GitHub allows 60 of those requests an hour for each IP address, shared with everything else behind that address. A token raises the limit to the token's own quota, 5,000 an hour for a personal access token. A fine-grained token with read-only access to public repositories is enough.

The server reads `GH_TOKEN`, or `GITHUB_TOKEN` when `GH_TOKEN` is unset or empty. It sends the value exactly as set, so a stray line break makes every GitHub request fail. The startup log names the variable it used, never the value. The token is sent only to `api.github.com`. Signing in with `gh auth login` inside a tab does not give the server a token, because the server reads only its own environment.

Every tab inherits the server's environment, so `gh` and other programs in a terminal can read the token too. Choose its access with that in mind. [Tools](tools.md#githubs-request-limit) describes what happens when the limit is reached.

## TRUSTED_INSTALL_UIDS

Before installing kiro-cli, and before the tools engine starts, the server checks who can write each folder on the way to `/config/tools`. It refuses when another account can, because what lands there later runs as root. The same list applies to both checks. Leave this unset, which makes no exception, for almost every setup. Set it only when the check refuses a volume you know is safe.

Each user ID you list states that the account is already as privileged as this server. Listing an account that is not hands it a way to run code as root. An entry that is not a whole number above `0` is skipped with a warning that names the variable and how many entries were dropped.

## KIRO_CLI_SANDBOX

Set it to `true` once the container runs with both the seccomp and AppArmor profiles from [Command sandbox](sandbox.md), which needs a host with AppArmor. At each start the server then checks that the container runs under that AppArmor profile, runs one command in kiro-cli's sandbox, and logs whether both worked. Only when it worked does kiro-cli get a working bubblewrap, so `/sandbox` does not report a sandbox that cannot run.

`true`, `yes`, `on` and `1` turn it on, in any letter case, and `false`, `no`, `off` and `0` leave it off. Any other value also leaves it off, with a warning. `/healthz` reports the result in its `sandbox` field.

## Uploads

When you paste an image into a tab, the page uploads it to `/uploads` and types its path at the prompt, followed by a space. kiro-cli attaches the image when you send the message. Paste with Ctrl+V, Cmd+V or Ctrl+Shift+V, or use the Paste menu on an iPhone or iPad. Text pastes as before.

The container image creates the folder itself, so pasting works with no volume. Without a volume, though, the files are lost when the container is recreated. Mount a volume, writable by the user the container runs as, to keep them.

Each image is named `pasted-<date>T<time>` in UTC, ending in `.png`, `.jpg` or `.webp`. Ctrl+Shift+V reads the clipboard directly, which a browser allows only over HTTPS or on `localhost`. Elsewhere it shows `Clipboard unavailable`, and Ctrl+V and Cmd+V still paste.

The browser scales an image larger than 2000 pixels on its long edge down to 2000 before the upload. It refuses an image it cannot convert to one of those three types. One paste uploads at most 25 images, and the page says how many it skipped. The server refuses a paste larger than 256 MiB. The Paste item in the right-click and long-press menu still pastes text only.
