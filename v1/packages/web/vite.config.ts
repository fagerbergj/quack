import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// A local distributor (PORT=8080); /sessions also carries the /ws upgrade.
const target = process.env.DISTRIBUTOR ?? 'http://127.0.0.1:8080';

export default defineConfig({
  base: './',
  plugins: [react(), tailwindcss()],
  server: { proxy: { '/sessions': { target, ws: true }, '/agents': target, '/index': target } },
  test: { environment: 'jsdom', setupFiles: ['./src/setup.ts'] },
});
