import { writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { promisify } from 'node:util';
import { brotliCompress, constants, gzip } from 'node:zlib';
import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import type { Plugin } from 'vite';
import { defineConfig } from 'vitest/config';

// A local distributor (PORT=8080); /sessions also carries the /ws upgrade.
const target = process.env.DISTRIBUTOR ?? 'http://127.0.0.1:8080';
const [br, gz] = [promisify(brotliCompress), promisify(gzip)];

// Writes .br and .gz beside each text output, so the distributor serves them without compressing per request.
const precompress = (): Plugin => ({
  name: 'quack-precompress',
  apply: 'build',
  async writeBundle({ dir = 'dist' }, bundle) {
    const outputs = Object.values(bundle).filter((o) => /\.(js|css|html|svg|json)$/.test(o.fileName));
    await Promise.all(outputs.map(async (o) => {
      const data = Buffer.from(o.type === 'chunk' ? o.code : o.source);
      if (data.length < 1024) return;
      const file = join(dir, o.fileName);
      await writeFile(`${file}.br`, await br(data, { params: { [constants.BROTLI_PARAM_QUALITY]: 11 } }));
      await writeFile(`${file}.gz`, await gz(data, { level: 9 }));
    }));
  },
});

export default defineConfig({
  base: './',
  plugins: [react(), tailwindcss(), precompress()],
  server: { proxy: { '/sessions': { target, ws: true }, '/agents': target, '/index': target, '/apps': target } },
  test: { environment: 'jsdom', setupFiles: ['./src/setup.ts'] },
});
