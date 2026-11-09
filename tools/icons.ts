// Renders packages/web/public/favicon.svg to the PNG icons with Playwright's Chromium: `node tools/icons.ts`.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright';

const PUB = fileURLToPath(new URL('../packages/web/public/', import.meta.url));
const SVG = readFileSync(`${PUB}favicon.svg`, 'utf8');
const TILE = '#1f7a63';

// scale: the mark's share of the icon; bg: a full-bleed fill, or none for transparent corners.
const ICONS = [
  { file: 'favicon.png', size: 48, scale: 1 },
  { file: 'icon-192.png', size: 192, scale: 1 },
  { file: 'icon-512.png', size: 512, scale: 1 },
  { file: 'apple-touch-icon.png', size: 180, scale: 1, bg: TILE },
  { file: 'icon-maskable-512.png', size: 512, scale: 0.8, bg: TILE },
];

// The farthest point of the duck's outlines from the icon centre, as a share of the icon size.
const reach = (size: number) => {
  const far = [...document.querySelectorAll('path')].flatMap((p) => {
    const n = p.getTotalLength();
    return Array.from({ length: 400 }, (_, i) => p.getPointAtLength((n * i) / 399).matrixTransform(p.getScreenCTM()!));
  });
  return Math.max(...far.map((pt) => Math.hypot(pt.x - size / 2, pt.y - size / 2))) / size;
};

const browser = await chromium.launch({ executablePath: process.env.QA_CHROMIUM || undefined });
try {
  for (const { file, size, scale, bg } of ICONS) {
    const page = await browser.newPage({ viewport: { width: size, height: size } });
    const px = size * scale;
    await page.setContent(`<style>body{margin:0;display:grid;place-items:center;height:100vh;background:${bg ?? 'transparent'}}svg{width:${px}px;height:${px}px}</style>${SVG}`);
    // A maskable icon's launcher crops to a circle of radius 40%, so the duck must fit inside it.
    if (file.includes('maskable') && (await page.evaluate(reach, size)) > 0.4) throw new Error(`${file}: the duck leaves the safe zone`);
    await page.screenshot({ path: `${PUB}${file}`, omitBackground: !bg });
    await page.close();
    console.log(`${file} ${size}x${size}`);
  }
} finally {
  await browser.close();
}
