# How Web Terminal for Kiro works

This page covers how the kiro-cli install, the health check and the startup log work, and which projects the app is built on, for readers running or debugging it.

## One process per tab

A single Go server serves the web page and starts one `kiro-cli chat` process per browser tab, each in its own terminal. The browser draws kiro-cli's own screen from that terminal's output, the way an SSH session would. There is no chat layer, no history database and no translation in between. Sessions stay alive with no browser attached, which is what lets a tab come back after sleep. They end when you close the tab or the container restarts.

## Sign-in

When a tab opens and kiro-cli is not signed in, the tab runs kiro-cli's device sign-in first. It prints an address and a one-time code. Open the address in any browser, your phone included, and enter the code, and the chat then starts in the same tab. Your sign-in, settings and installed tools live on the `/config` volume and survive recreating the container.

## The kiro-cli install

kiro-cli is pinned to one version, downloaded on first start and checked against a pinned digest. It is not redistributed inside the image, and a newer version arrives by pulling a newer image tag. Automatic updates inside kiro-cli are turned off, because a binary that replaces itself would no longer match the digest.

The download runs after the server starts listening, so the web page and `/api/health` answer at once. New tabs answer `503 {"reason":"kiro-cli installing"}` until the install finishes, and the reason names the stage when something goes wrong.

`/config` must be on a filesystem that allows running programs. kiro-cli and every tool the container installs live there and run from there, so a `noexec` mount makes the install fail. The startup log reports that in one line that names the folder.

If an install fails for good, for example with no network on first start or a full disk, the container stays up so you can repair it. To retry the download, fix the cause and restart the container. If you repaired or restored a complete version folder under `/config/tools/kiro-cli-versions` in place, run `curl -X POST localhost:9848/api/kiro-cli/rescan` inside the container instead. It checks what is on disk again and downloads nothing. That address answers only from inside the container, like the tools commands.

## init: true

The compose file sets `init: true`, and it is required. A kiro-cli session starts language servers, `git` processes and node runtimes whose own parent exits. Those orphans are handed to the process with ID 1, and the server waits only for the processes it started itself. Without an init, the server is process 1, and the orphans pile up as dead entries for the life of the container. The server logs a warning at startup when it runs as process 1, so a setup that leaves the line out does not fail silently.

## Health

The image checks `/api/health` on loopback every 30 seconds, after a 20-minute start period that covers the first kiro-cli download. The check reports whether kiro-cli is ready, not only whether the server is listening, and a 503 body names the state. It is a readiness check. Nothing restarts on an unhealthy state, so a broken install shows as `unhealthy` in `docker ps` without a restart loop. The tool fields in the same answer are described in [Tools](tools.md#when-tabs-open).

## Startup failures

A startup failure writes exactly one `ERROR` line, `web-terminal-kiro exited with error`. Its `error` field carries the remedy, and its `stage` field names the step that failed:

| Stage | Meaning |
| --- | --- |
| `work_dir` | The `/workspace` mount is missing or is not a folder. |
| `static` | The web page built into the image is unusable, which is a build defect. |
| `listen` | The port could not be opened. |
| `serve` | The web server stopped with an error. |
| `unknown` | A failure no stage covers. |

Base log searches and alert rules on `stage`, not on the message text. The stage values stay the same between releases, and the wording may change.

## Related projects

- [web-terminal-engine](https://github.com/cplieger/web-terminal-engine) is the terminal engine behind this app, a Go server half and a TypeScript drawing half.
- [web-terminal-ui](https://github.com/cplieger/web-terminal-ui) is the touch-first browser page this app is built on.
- [pinstall](https://github.com/cplieger/pinstall) is the installer this app uses to download, verify and activate kiro-cli at runtime.
