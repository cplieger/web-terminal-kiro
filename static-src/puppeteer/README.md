# Puppeteer smoke harness

Scaffolding for running ad-hoc browser-driven smoke / repro tests
against a real vibecli + Chromium. Not part of the regular test
suite — `*.cjs` test files in this directory are gitignored and
exist only when someone is actively diagnosing something.

## One-time setup

```sh
# from /workspace/vibecli
cd static-src
npm install --no-save puppeteer typescript

# OS deps for the bundled chromium (Debian trixie)
apt-get install -y \
    libglib2.0-0 libnss3 libnspr4 libdbus-1-3 libatk1.0-0 \
    libatk-bridge2.0-0 libcups2 libgtk-3-0 libgbm1 libxss1 \
    libxcomposite1 libxdamage1 libxrandr2 libxkbcommon0 \
    libpango-1.0-0 libcairo2 fonts-liberation libdrm2 \
    libxshmfence1 libatspi2.0-0 libasound2t64
```

## Build the bundle the browser will load

```sh
# from /workspace/vibecli/static-src
node_modules/.bin/tsc --project tsconfig.json
# Concatenate the CSS bundle (mirrors what the Dockerfile does).
cd ..
> static/style.css
while IFS= read -r line; do
    case "$line" in ''|\#*) continue ;; esac
    cat "static-src/css/${line}" >> static/style.css
done < static-src/css/MANIFEST
go build -o /tmp/vibecli .
```

## Write a test

```js
// /workspace/vibecli/static-src/puppeteer/my_test.cjs
const puppeteer = require('../node_modules/puppeteer');
const { spawn } = require('child_process');
const PORT = 19848;

const vibecli = spawn('/tmp/vibecli', [], {
  env: {
    ...process.env,
    KWEB_ADDR: `:${PORT}`,
    // Use the bash wrapper for fast, deterministic boot. Replace with
    // /config/tools/bin/kiro-cli for kiro-specific scenarios.
    KIRO_CLI_PATH: __dirname + '/bash-wrap.sh',
    KWEB_WORK_DIR: '/tmp',
  },
  stdio: ['ignore', 'pipe', 'pipe'],
});
vibecli.stderr.pipe(process.stderr);

(async () => {
  // wait for vibecli to be reachable on PORT, then puppeteer.launch(),
  // page.goto('http://127.0.0.1:' + PORT + '/'), drive the page...
  // ...
  vibecli.kill('SIGTERM');
})();
```

Run with `node static-src/puppeteer/my_test.cjs`.

## Notes

- kiro-cli takes 10-15s to boot in headless Chromium because of its
  auth + LSP startup. For tests that don't need kiro specifically,
  prefer `bash-wrap.sh` — bash boots instantly.
- For kiro-specific tests, pre-warm via a Go WS client first so the
  vibecli VT screen has the prompt in place before puppeteer attaches.
- The Nerd Font 404s in dev (it's only bundled inside the Docker
  image), which gates `fontsLoaded` and prevents the first resize
  from being sent. Workaround: send Enter once at test start to kick
  bash into life via the input path (server starts the inner cmd on
  first input as a fallback).
