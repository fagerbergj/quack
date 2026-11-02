import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { expect, test } from 'vitest';

const pub = resolve(__dirname, '../public');
const manifest = JSON.parse(readFileSync(resolve(pub, 'manifest.webmanifest'), 'utf8'));

test('the manifest is installable under a path prefix and its icons exist', () => {
  expect(manifest).toMatchObject({ name: 'quack', display: 'standalone', start_url: './', scope: './' });
  const sizes = manifest.icons.map((i: { sizes: string; purpose: string }) => `${i.sizes} ${i.purpose}`);
  expect(sizes).toEqual(expect.arrayContaining(['192x192 any', '512x512 any', '512x512 maskable']));
  for (const { src } of manifest.icons) expect(existsSync(resolve(pub, src)), src).toBe(true);
});
