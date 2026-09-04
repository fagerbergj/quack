import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  test: {
    // render-check.browser.test.tsx needs Vitest browser mode (its own
    // vitest.render-check.config.ts, run via `npm run render-check`) - the
    // default `npm test` run is node/jsdom-env and can't load `vitest/browser`.
    exclude: ['**/node_modules/**', 'src/render-check.browser.test.tsx'],
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
