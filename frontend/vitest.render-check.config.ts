import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { playwright } from '@vitest/browser-playwright'

// Separate config from the default `npm test` (node-env, logic-only per
// frontend-design skill) - this one drives a real Chromium via Playwright to
// catch what tsc/eslint/vitest/build cannot: stray JSX text nodes, dialog
// stacking-context bugs, and horizontal overflow (#1192).
export default defineConfig({
  plugins: [react()],
  test: {
    include: ['src/render-check.browser.test.tsx'],
    browser: {
      enabled: true,
      provider: playwright(),
      headless: true,
      instances: [{ browser: 'chromium' }],
    },
    // A viewport/theme x every story combination is a lot of Chromium round
    // trips - single worker keeps screenshot capture and elementFromPoint
    // reads deterministic (no cross-test viewport races in one shared page).
    fileParallelism: false,
  },
})
