// Vitest 4.1 configuration for vibecli TypeScript unit tests.
// Default environment: node (pure functions, no DOM overhead).
// DOM modules: add `// @vitest-environment happy-dom` at the top of the
// test file to get window/document/localStorage/etc. No browser binary
// needed — happy-dom is a pure JS DOM implementation running in Node.
// Run: vitest --run (single pass) or vitest (watch mode)
import { configDefaults, defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // Default: node. Override per test file with:
    //   // @vitest-environment happy-dom
    environment: "node",

    // threads pool: faster than forks for pure Node.js tests with no native
    // modules.
    pool: "threads",

    // Disable test isolation: pure functions have no side effects, so
    // isolation adds overhead with no benefit.
    isolate: false,

    // Test files co-located with source, named *.test.ts
    include: ["**/*.test.ts"],

    // Exclude compiled output, plus Vitest's defaults (node_modules at
    // any depth, .git). Spreading configDefaults.exclude avoids narrowing
    // the built-in "**/node_modules/**" to a top-level-only glob.
    // "**/.code-review/**" keeps stray *.test.ts scratch (e.g. files written under
    // .code-review/tmp by tooling) from being collected and failing the run.
    exclude: [...configDefaults.exclude, "../static/**", "**/.code-review/**"],

    // Forbid .only tests unconditionally — not just in CI.
    allowOnly: false,

    // app.test.ts covers vibecli's thin bootstrap (the mount() wiring); the terminal
    // logic itself is tested in @cplieger/web-terminal-ui and @cplieger/web-terminal-engine.
    // passWithNoTests stays as a safety net so moving the bootstrap test into the
    // packages later won't hard-fail the suite here.
    passWithNoTests: true,

    // Require explicit imports of describe/it/expect from "vitest".
    globals: false,

    // Force every test to call at least one expect(). Catches tests that
    // accidentally pass because they never assert anything.
    expect: {
      requireAssertions: true,
    },

    // Auto-clean/reset/restore all mocks and stubs before each test.
    clearMocks: true,
    mockReset: true,
    restoreMocks: true,
    unstubEnvs: true,
    unstubGlobals: true,

    // Stop after the first failure in CI; collect full results locally.
    bail: process.env["CI"] ? 1 : 0,

    // Per-test timeout. Pure-function tests should never need more than 2s.
    testTimeout: 2000,
    hookTimeout: 5000,

    // Flag tests slower than 100ms — pure functions have no I/O.
    slowTestThreshold: 100,

    // Reproducible ordering. hooks: "stack" = afterEach/afterAll run in
    // reverse definition order (correct teardown semantics).
    sequence: {
      shuffle: { files: false, tests: false },
      concurrent: false,
      hooks: "stack",
    },

    // Print stack traces with every console.* call in tests.
    printConsoleTrace: true,

    // Show full diff when a snapshot fails.
    expandSnapshotDiff: true,

    // V8 coverage with AST-accurate remapping.
    coverage: {
      provider: "v8",
      include: ["*.ts"],
      exclude: ["*.test.ts", "*.d.ts"],
      reportOnFailure: true,
      reporter: ["text", "text-summary", "lcov"],
      thresholds: {
        // Thresholds intentionally low: vibecli's frontend is small and
        // most code is DOM/terminal/WS side-effect wiring. Bump these
        // when more pure helpers are extracted into testable modules.
        lines: 5,
        functions: 5,
        branches: 5,
        statements: 5,
      },
    },

    chaiConfig: {
      truncateThreshold: 0,
      showDiff: true,
      includeStack: true,
    },

    experimental: {
      fsModuleCache: true,
      fsModuleCachePath: ".vitest-cache",
    },
  },
});
