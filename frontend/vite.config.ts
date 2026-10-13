import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { configDefaults } from 'vitest/config'
import { createHash } from 'node:crypto'

// The SPA build id hashes the content-hashed asset names, so it moves exactly when the shipped bundle does
// and needs no git checkout (the Docker context excludes .git).
function buildId(bundle: Record<string, unknown>): string {
  const names = Object.keys(bundle).filter(n => n.startsWith('assets/')).sort()
  return createHash('sha256').update(names.join('\n')).digest('hex').slice(0, 12)
}

// Stamps the id into index.html (the page's own) and version.json (what is served now; see clientConfig.ts).
const versionJson: Plugin = {
  name: 'quack-version-json',
  apply: 'build',
  transformIndexHtml: {
    order: 'post',
    handler: (_html, ctx) => ctx.bundle
      ? [{ tag: 'meta', attrs: { name: 'quack-build', content: buildId(ctx.bundle) }, injectTo: 'head' }]
      : [],
  },
  generateBundle(_opts, bundle) {
    this.emitFile({ type: 'asset', fileName: 'version.json', source: JSON.stringify({ build: buildId(bundle) }) })
  },
}

export default defineConfig({
  plugins: [react(), tailwindcss(), versionJson],
  test: {
    // render-check.browser.test.tsx needs Vitest browser mode (its own
    // vitest.render-check.config.ts, run via `npm run render-check`) - the
    // default `npm test` run is node/jsdom-env and can't load `vitest/browser`.
    // Extends (not replaces) Vitest's own defaults - a bare array here would
    // drop dist/cypress/config-file exclusions too.
    exclude: [...configDefaults.exclude, 'src/render-check.browser.test.tsx'],
    setupFiles: ['vitest.setup.ts'],
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
