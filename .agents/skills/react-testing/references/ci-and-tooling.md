# CI/CD, config, parallelism, coverage

## Vitest vs Jest

| Concern | Vitest | Jest (SWC) |
|---|---|---|
| Cold run (full suite) | ~14s | ~19s |
| Watch re-run (1 file) | ~0.18s | ~1.4s |
| Memory / worker | ~110 MB | ~150 MB |
| ESM | native, no flags | experimental `--experimental-vm-modules` |
| TypeScript | esbuild built-in | SWC or ts-jest |
| Config reuse | shares `vite.config.ts` | standalone `jest.config.js` |

- **New Vite/Nuxt/SvelteKit/ESM project → Vitest.** **React Native, large legacy webpack, deep Jest investment → Jest.**
- Vitest mirrors Jest's API; migrate via codemod. Friction: ESM mock hoisting (`vi.hoisted()`) and RN presets (Jest-first).

## Vitest config

```ts
// vitest.config.ts
import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['./src/setupTests.ts'],
    coverage: {
      provider: 'v8',                       // or 'istanbul' (slower, more precise)
      reporter: ['text', 'html', 'lcov'],
      thresholds: { lines: 90, functions: 90, branches: 85, statements: 90 },
    },
  },
});
```

## Jest config

```ts
// jest.config.ts
import type { Config } from 'jest';
const config: Config = {
  testEnvironment: 'jsdom',
  setupFilesAfterEnv: ['./src/setupTests.ts'],
};
export default config;
```

```js
// coverage in jest.config
collectCoverageFrom: ['src/**/*.{js,jsx,ts,tsx}', '!src/**/*.d.ts'],
coverageThreshold: { global: { branches: 80, functions: 80, lines: 85, statements: 85 } },
```

## setupTests

```ts
import '@testing-library/jest-dom';        // toBeInTheDocument, etc.
// Jest + React 18 only — silence act() warnings:
global.IS_REACT_ACT_ENVIRONMENT = true;
```

## CI commands

- Jest: `jest --ci --coverage --reporters=default --reporters=jest-junit`
- Vitest: `vitest run --coverage` (built-in JUnit reporter)

## Parallelism

**Vitest pools:**
- `pool: 'threads'` (default) — worker threads share module caches; fastest, but can leak state via module singletons.
- `pool: 'forks'` — process per file; full isolation, cold-start cost.
- `isolate: true` (default) gives each file a fresh module graph. `isolate: false` ≈ 30% faster but out-of-order failures from leaked mutations.

```ts
test: {
  pool: 'threads',
  poolMatchGlobs: [['**/integration/**/*.test.ts', 'forks']], // isolate expensive suites only
}
```

**Jest:** default `maxWorkers` = logical CPUs, isolated process per file. Sharding (Jest 28+) splits across CI machines: `jest --ci --shard=1/4`.

## Coverage targets (industry baseline)

| Metric | Target |
|---|---|
| Statements | 80%+ |
| Branches | 75%+ |
| Functions | 80%+ |
| Lines | 80%+ |

Threshold breach → exit code 1 → CI fails. Report `lcov`; integrate Codecov/Coveralls + PR comments.
