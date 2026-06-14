# Roadmap

Vibecli is **pre-1.0** (currently `v0.x`) — a minimal browser terminal wrapping
a single kiro-cli PTY. This document supersedes the
[default fleet roadmap](https://github.com/cplieger/.github/blob/main/ROADMAP.md)
for this repository.

## Current focus

- **iOS UI / UX.** The bulk of the remaining work is on the iOS browser
  experience: terminal rendering, keyboard handling, and touch interaction on
  mobile Safari.
- **WebSocket robustness.** Resolve the outstanding issues around the PTY
  WebSocket stream — reconnection, backgrounding/foregrounding on mobile, and
  stream stability — which are the main source of current bug reports.

## Ongoing

- Incorporate fixes from the weekly central fuzzing and
  [gremlins](https://gremlins.dev/) mutation-testing runs.
- Adopt the stable `tsgo`
  ([@typescript/native-preview](https://www.npmjs.com/package/@typescript/native-preview))
  release for the browser terminal frontend once it reaches general
  availability.
- Track the pinned kiro-cli version via Renovate; dependency and base-image
  currency; security findings (CodeQL / Trivy / Scorecard) addressed as they
  arise.
- Bug and security response per
  [SECURITY.md](https://github.com/cplieger/.github/blob/main/SECURITY.md).
