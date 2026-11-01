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
