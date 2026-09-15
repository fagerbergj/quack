import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { configDefaults } from 'vitest/config'

export default defineConfig({
  plugins: [react()],
  test: {
    // render-check.browser.test.tsx needs Vitest browser mode (its own
    // vitest.render-check.config.ts, run via `npm run render-check`) - the
    // default `npm test` run is node/jsdom-env and can't load `vitest/browser`.
    // Extends (not replaces) Vitest's own defaults - a bare array here would
    // drop dist/cypress/config-file exclusions too.
    exclude: [...configDefaults.exclude, 'src/render-check.browser.test.tsx'],
    coverage: {
      provider: 'v8',
      // lcov carries per-line hit counts for the changed-line coverage gate
      // (scripts/coverage-diff.mjs); json-summary is just the rollup.
      reporter: ['lcov', 'json-summary'],
      include: ['src/**/*.{ts,tsx}'],
      exclude: ['src/generated/**'],
    },
  },
  server: {
    port: 3000,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
