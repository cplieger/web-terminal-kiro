# Configuration

This page lists every setting of Web Terminal for Kiro and what each one changes, for readers who want more than the quick start sets.

## Where settings live

Settings are environment variables on the container, under `environment:` in `compose.yaml`. The server reads them once at start, so recreate the container after a change. No setting is required, and the image ships working defaults. A malformed value logs a warning that names the variable, never the value, and the server falls back to the default. `ALLOWED_HOSTS` is the exception, described below.

## Every setting

| Variable | Description | Default |
| --- | --- | --- |
| `ALLOWED_HOSTS` | Comma-separated hostnames and IPs you open the terminal at. Any other host name is refused. Unset accepts every host and logs a warning. | _(unset)_ |
| `TRUSTED_PROXIES` | Reverse-proxy CIDRs or IPs whose `X-Forwarded-For` the access log trusts to name the real client. | _(unset)_ |
| `LISTEN_ADDR` | Listen address as `host:port`. Keep the host part empty so the healthcheck can still reach it on `127.0.0.1`. | `:9848` |
| `KIRO_CLI_CHAT_ARGS` | Extra flags added to every tab's `kiro-cli chat` command, separated by spaces, for example `--v3`. | _(unset)_ |
| `LOG_LEVEL` | `debug`, `info`, `warn` or `error`. An unknown value falls back to `info` with a warning. | `info` |
| `WORK_DIR` | The folder each new tab starts in. It must exist. | `/workspace` |
| `SCROLLBACK` | Lines of history kept per tab. kiro-cli wipes history when it redraws the whole screen, so in practice a tab keeps about 3000 lines. | `100000` |
| `TOOL_CATALOG_REFRESH` | How often the tool catalog is refreshed, as a duration such as `24h`. `off` or `0` stops the schedule. | `24h` |
| `TOOL_CATALOG_URL` | Where catalog refreshes come from. Point it at a fork or a mirror of the catalog. | the [tool-catalog](https://github.com/cplieger/tool-catalog) latest release |
| `TOOL_CATALOG_PATH` | The catalog built into the image, used at first start and offline until a fetched catalog replaces it. | `/app/tool-catalog.json` |
| `BUNDLED_TOOLS_PATH` | The file in the image naming the four language servers no registry carries, merged over every loaded catalog. | `/app/bundled-tools.json` |
| `GH_TOKEN` | A GitHub token for tool version checks, which raises GitHub's limit of 60 requests an hour. Every tab sees it. Takes precedence over `GITHUB_TOKEN`. | _(unset)_ |
| `GITHUB_TOKEN` | Read in place of `GH_TOKEN` when that one is unset or empty. | _(unset)_ |
| `TRUSTED_INSTALL_UIDS` | Numeric user IDs allowed to write to the kiro-cli install folder. Leave it unset unless the install check refuses a volume you know is safe. | _(unset)_ |
| `LOG_OSC_TEXT` | Log the text of unrecognized terminal notifications at `debug`. That text can hold a token, so turn it on only while diagnosing. | `false` |

## ALLOWED_HOSTS

List the exact names and addresses you type in the browser, for example `localhost,192.168.1.5,webterm.example.com`. Use bare names and addresses only, with no scheme, path or CIDR range such as `10.0.0.0/8`. This list is the check that stops DNS rebinding, described in [Security](hardening.md).

A request that is loopback on both ends is always admitted, so the healthcheck and commands run inside the container keep working. Both the client address and the `Host` must be loopback for that, as with `127.0.0.1:9848` or `localhost:9848`. Any other name still needs to be in the list.

Malformed entries are dropped with a warning that gives their count. When every entry is malformed, the server refuses every request that is not loopback and logs why.

## TRUSTED_PROXIES

With this unset, the access log records the address that opened the connection and ignores `X-Forwarded-For`, so nobody can fake the logged address. That is the right choice when the terminal is reached directly. Behind a proxy, set it to the proxy's addresses, for example `10.0.0.0/8,192.0.2.10`. A malformed entry is logged and skipped, and startup continues. A default route such as `0.0.0.0/0` logs a warning, because it treats every client as a proxy.

## LISTEN_ADDR

The image's healthcheck calls `127.0.0.1` on the port taken from this value. A listen address pinned to one interface that is not loopback therefore reports the container `unhealthy` while it serves normally. Limit who can reach the port with the published port, for example `127.0.0.1:9848:9848`, or with a reverse proxy.

## KIRO_CLI_CHAT_ARGS

Use it for any flag `kiro-cli chat` accepts, for example `--effort high` or `--v3`. The flags go only to `kiro-cli chat`, not to the sign-in commands. Their values never reach the log. The startup line records only how many flags there are.

## LOG_LEVEL

The value is read in any case, so `DEBUG` works too. `debug` adds the diagnostics behind each tab's status dot, which help when a dot stays stuck.

## SCROLLBACK

History is held in memory and grows as a session produces it, so a large value costs nothing until a tab reaches it. kiro-cli clears its history on every full-screen repaint. A tab therefore keeps roughly 3000 lines whatever you set here, and raising it gains nothing. `0` keeps nothing beyond the live screen. A value from `1` to `2000` is raised to `2001` with a warning. Below that depth, the browser keeps its whole buffer instead. This variable has the same name and meaning in every app built on [web-terminal-engine](https://github.com/cplieger/web-terminal-engine).

## TOOL_CATALOG_REFRESH

With the schedule off, a refresh still runs when a command inside the container asks for one, as `curl -X POST localhost:9848/api/tools/catalog/refresh`. [Tools](tools.md) covers the catalog.

## GH_TOKEN

The tools engine asks GitHub's API for the latest version of each tool and for the files of a GitHub release. Without a token, GitHub allows 60 of those requests an hour for each IP address, shared with everything else behind that address. A token raises the limit to the token's own quota, 5,000 an hour for a personal access token. A fine-grained token with read-only access to public repositories is enough.

The server reads `GH_TOKEN`, or `GITHUB_TOKEN` when `GH_TOKEN` is unset or empty. It sends the value exactly as set, so a stray line break makes every GitHub request fail. The startup log names the variable it used, never the value. The token is sent only to `api.github.com`. Signing in with `gh auth login` inside a tab does not give the server a token, because the server reads only its own environment.

Every tab inherits the server's environment, so `gh` and other programs in a terminal can read the token too. Choose its access with that in mind. [Tools](tools.md#githubs-request-limit) describes what happens when the limit is reached.

## TRUSTED_INSTALL_UIDS

Before installing kiro-cli, the server checks who can write each folder on the way to the install under `/config/tools`. It refuses when another account can, because what lands there later runs as root. Leave this unset, which makes no exception, for almost every setup. Set it only when the check refuses a volume you know is safe.

Each user ID you list states that the account is already as privileged as this server. Listing an account that is not hands it a way to run code as root. An entry that is not a whole number above `0` is skipped with a warning that names the variable and how many entries were dropped.

## LOG_OSC_TEXT

Any program running in the terminal can send a notification, so the text can hold a token, a sign-in code or an address with a token in it. With this off, the log still records a fingerprint and a length for each wording. Turn it on only for an active diagnosis. The server logs a warning at startup while it is on.
