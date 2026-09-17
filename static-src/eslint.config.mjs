// App-local ESLint config for web-terminal-kiro's front end.
//
// The shared ruleset lives in ./eslint.config.base.mjs; this file imports it and
// layers on the one repo-specific delta, the scratch trees below.
//
// The base sits in THIS directory rather than the repo root, and that is what
// makes importing it possible at all. Node resolves a bare specifier from the
// importing module's own directory upward, so while the base lived at the repo
// root it could not resolve its own `@eslint/js` / `typescript-eslint` imports --
// this app's dependencies are installed here, beside its package.json:
//
//   Error [ERR_MODULE_NOT_FOUND]: Cannot find package '@eslint/js'
//     imported from <repo>/eslint.config.base.mjs
//
// It also fixes the base's `tsconfigRootDir: import.meta.dirname`, which from
// here resolves to the directory that actually holds tsconfig.json.
//
// THIS COPY IS HAND-MAINTAINED. sync.yaml writes cplieger/ci's canonical config
// to <repo>/eslint.config.base.mjs, the ROOT, which nothing reads (the
// package-dir dest was tried in ci #371 and reverted by #372). So a canonical
// improvement lands in the unread root file and does NOT reach this one: when
// the root copy changes, copy it here too. Measured 2026-09-16, this file had
// been 3 lines stale since ci 1fa49d5. Nothing detects that.
import baseConfig from "./eslint.config.base.mjs";

export default [
  ...baseConfig,
  {
    // Stryker's sandbox + report output and the Kiro skill scratch trees. All are
    // gitignored, but ESLint flat config does not read .gitignore (and no longer
    // ignores dot-directories by default), so a leftover sandbox (an interrupted
    // mutation run never cleans it up) or a review run makes `npm run lint:eslint`
    // fail on hundreds of copied, @ts-nocheck-stamped files. .prettierignore and
    // vitest.config.ts exclude the same trees for the same reason.
    //
    // App-local by design, not a candidate for the canonical config: these are
    // this repo's scratch trees. The equivalent parity problem in the CENTRAL
    // lint steps (stylelint, html-validate) is solved where it belongs, in
    // cplieger/ci's _ci_local.py gitignore rewrites, not by pushing app scratch
    // names into a fleet-wide config.
    ignores: ["**/.stryker-tmp/**", "**/reports/**", "**/.code-review/**"],
  },
];
