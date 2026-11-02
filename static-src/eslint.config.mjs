// The shared ruleset is ./eslint.config.base.mjs, synced from cplieger/ci;
// this file adds the scratch trees below.
import baseConfig from "./eslint.config.base.mjs";

export default [
  ...baseConfig,
  {
    // Stryker's sandbox and report output and other scratch trees. All are
    // gitignored, but ESLint flat config does not read .gitignore, so a leftover
    // scratch tree makes `npm run lint:eslint` fail on hundreds of copied,
    // @ts-nocheck-stamped files. .prettierignore and vitest.config.ts
    // exclude the same trees. App-local by design: these are this repo's scratch
    // names and do not belong in the shared canonical config.
    ignores: ["**/.stryker-tmp/**", "**/reports/**", "**/.code-review/**"],
  },
];
