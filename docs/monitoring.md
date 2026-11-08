# Monitoring and alerts

This page says how to know that Web Terminal for Kiro is up and what its log records, for anyone who runs it. The terminal serves no metrics page and ships no alert rules, because a person is at the terminal whenever it matters. An uptime prober on `/healthz` tells you when it is down.

## What you get

| You run | You get | You set up |
| --- | --- | --- |
| Prometheus, Mimir or VictoriaMetrics, with Alertmanager | An alert when `/healthz` stops answering `200`. This app ships no rules of its own | A probe of `/healthz` and the health page rule from the monitoring guide's [Knowing that an app is alive](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#knowing-that-an-app-is-alive) |
| Loki and Alertmanager, no metric store | No alerts. The log records each failure, and no rule reads it | An uptime prober on `/healthz` |
| Docker or Unraid only | Docker health, which notifies nobody | An uptime prober on `/healthz`, or something that watches Docker health |

## Logs

The server and the container's start script both write logfmt lines to standard error. The start script's lines carry `component=entrypoint`. `LOG_LEVEL` sets the server's level, `info` by default.

No alert rule reads the log. Search it by the `stage` key when the server does not start: its one `ERROR` line, `web-terminal-kiro exited with error`, carries `stage` and the remedy in `error`. [Startup failures](how-it-works.md#startup-failures) lists the stage values, which stay the same between releases. The `terminal attach attempt` line records each request that presents a session id, with the client address and request id.

A healthy probe of `/healthz` logs nothing at `info`. A probe that gets `503` logs one access line at `ERROR`, and one refused with a `4xx` code logs it at `WARN`.

## Knowing the app is alive

| Route | Method | Port | Login | Answers |
| --- | --- | --- | --- | --- |
| `/healthz` | `GET`, `HEAD` | `LISTEN_ADDR`, `9848` by default | none. `ALLOWED_HOSTS` applies, loopback always admitted | `200 {"status":"ok"}`, or `503 {"status":"unhealthy","reasons":[...]}` |

**What healthy means.** The server is listening, is not starting or shutting down, and has kiro-cli installed with its required settings applied. While kiro-cli downloads, retries its install or cannot be installed, `/healthz` answers `503`, and `reasons` names the state, such as `kiro-cli installing`. Any other method answers `405`. The answer also carries `tools`, `tools_missing` and `sandbox`, described in [Tools](tools.md#when-tabs-open) and [Command sandbox](sandbox.md#the-startup-check). Those fields never change the status code, so a failed tool install or an unreachable GitHub never makes the terminal unhealthy. It shows in `tools` and in the log.

**Docker health.** The image checks itself every 30 seconds with `curl`, which asks `/healthz` on `127.0.0.1` at the port in `LISTEN_ADDR` and gives up after 4 seconds. Docker marks the container unhealthy after 3 failures, and allows 20 minutes after a start, which covers the first kiro-cli download. Docker's health status notifies nobody on its own, and Docker does not restart an unhealthy container.

**Who is told.**

| You run | A dead terminal | A wedged terminal |
| --- | --- | --- |
| Prometheus with the guide's health page rule | the health page rule | the same, once `/healthz` stops answering `200` |
| Loki only | an uptime prober on `/healthz` | the same |
| Docker or Unraid only | an uptime prober on `/healthz`, or something that watches Docker health | the same |

A tab whose kiro-cli stops responding affects only that tab, with you in front of it. The server answers for every tab, so it stays healthy.

**Probing from outside.** Check `GET http://<host>:9848/healthz` and pass on status 200 only. Do not match the body. Use a 5-second timeout, check every 60 seconds, alert after 3 failures, and wait 20 minutes after a first start. After an image update that moves kiro-cli to a new version, the terminal downloads it again and answers `503` meanwhile, so hold the alert for that long or expect it once. Address the terminal by a name in `ALLOWED_HOSTS`, or it refuses the request with `403`. A reverse proxy that asks for a login refuses a prober that does not send it, so probe the port directly on your network, or give the prober the login. A Kubernetes liveness probe that must ignore readiness uses a `tcpSocket` probe on the port.
