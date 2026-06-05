import { defineConfig } from '@hey-api/openapi-ts'

// Generates the TypeScript client (types + SDK) from the single source of
// truth, ../openapi.yaml, into src/generated. Run with `npm run generate`.
export default defineConfig({
  input: '../openapi.yaml',
  output: 'src/generated',
  plugins: ['@hey-api/client-fetch', '@hey-api/typescript', '@hey-api/sdk'],
})
