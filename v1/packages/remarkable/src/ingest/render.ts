import { execFile } from 'node:child_process';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';
import { getDocumentProxy, renderPageAsImage } from 'unpdf';

// The tablet's own pixel width: handwriting stays legible and the image stays well under a vision model's token cap.
export const PAGE_WIDTH = 1404;

/** Every page of the PDF as a JPEG. */
export async function renderPages(pdf: Uint8Array): Promise<Uint8Array[]> {
  const doc = await getDocumentProxy(new Uint8Array(pdf));
  const pages: Uint8Array[] = [];
  for (let n = 1; n <= doc.numPages; n++) {
    pages.push((await renderPageAsImage(doc, n, { canvasImport: () => import('@napi-rs/canvas'), width: PAGE_WIDTH, format: 'jpeg', quality: 0.85 })) as Uint8Array);
  }
  return pages;
}

const run = promisify(execFile);
const SCRIPT = join(import.meta.dirname, 'rm_pages.py');

async function rasterize(svg: Uint8Array): Promise<Uint8Array> {
  const { createCanvas, loadImage } = await import('@napi-rs/canvas');
  const image = await loadImage(Buffer.from(svg));
  const canvas = createCanvas(PAGE_WIDTH, Math.round((PAGE_WIDTH * image.height) / image.width));
  const ctx = canvas.getContext('2d');
  ctx.fillStyle = '#fff';
  ctx.fillRect(0, 0, canvas.width, canvas.height);
  ctx.drawImage(image, 0, 0, canvas.width, canvas.height);
  return canvas.encode('jpeg', 85);
}

type Pages = { fileType: string; pages?: number; error?: { page: number; reason: string } };

async function draw(rmdoc: Uint8Array, dir: string): Promise<Pages> {
  const pending = run('python3', [SCRIPT, dir], { maxBuffer: 1 << 20 });
  pending.child.stdin!.on('error', () => {});
  pending.child.stdin!.end(rmdoc);
  try {
    return JSON.parse((await pending).stdout) as Pages;
  } catch (err) {
    const stderr = String((err as { stderr?: string }).stderr ?? '').trim();
    throw new Error(stderr.split('\n').at(-1) || (err as Error).message);
  }
}

/** Every page of a notebook in an rmfakecloud .rmdoc zip as a JPEG, drawn by rmc; undefined for a PDF or epub, which rmfakecloud's own export renders. */
export async function renderNotebook(rmdoc: Uint8Array, name: string): Promise<Uint8Array[] | undefined> {
  const dir = await mkdtemp(join(tmpdir(), 'quack-rm-'));
  try {
    const out = await draw(rmdoc, dir).catch((err: Error) => {
      throw new Error(`could not read ${name}: ${err.message}`);
    });
    if (out.error) throw new Error(`could not render page ${out.error.page} of ${name}: ${out.error.reason}`);
    if (out.pages === undefined) return undefined;
    const pages: Uint8Array[] = [];
    for (let n = 1; n <= out.pages; n++) {
      pages.push(
        await rasterize(await readFile(join(dir, `${n}.svg`))).catch((err: Error) => {
          throw new Error(`could not render page ${n} of ${name}: ${err.message}`);
        }),
      );
    }
    return pages;
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}
