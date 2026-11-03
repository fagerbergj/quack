import { useRef, useState, type ChangeEvent, type ClipboardEvent, type DragEvent } from 'react';

export type Img = { data: string; mimeType: string };

// Mirrors the session's caps (images.ts); the server enforces them, this only spares the upload.
export const MAX_IMAGES = 4;
const MAX_CHARS = 3 * 1024 * 1024;
const LONG_EDGE = 2048;

export const dataUrl = (i: Img) => `data:${i.mimeType};base64,${i.data}`;

// Re-encoded as JPEG at a sane size: phone photos are several MB, and canvas also applies the EXIF rotation.
async function downscale(file: File): Promise<Img> {
  const bitmap = await createImageBitmap(file);
  const scale = Math.min(1, LONG_EDGE / Math.max(bitmap.width, bitmap.height));
  const canvas = document.createElement('canvas');
  canvas.width = Math.round(bitmap.width * scale);
  canvas.height = Math.round(bitmap.height * scale);
  const g = canvas.getContext('2d')!;
  g.fillStyle = '#fff';
  g.fillRect(0, 0, canvas.width, canvas.height);
  g.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
  bitmap.close();
  return { data: canvas.toDataURL('image/jpeg', 0.85).split(',')[1], mimeType: 'image/jpeg' };
}

/** The images waiting to be sent, with the message to show when one cannot be added. */
export function useAttachments(initial: Img[] = []) {
  const [images, setImages] = useState<Img[]>(initial);
  const [problem, setProblem] = useState('');
  const add = async (files: File[]) => {
    const found = files.filter((f) => f.type.startsWith('image/'));
    if (!found.length) return;
    const room = MAX_IMAGES - images.length;
    setProblem(found.length > room ? `At most ${MAX_IMAGES} images per message.` : '');
    const added: Img[] = [];
    for (const file of found.slice(0, Math.max(room, 0))) {
      try {
        const img = await downscale(file);
        if (img.data.length > MAX_CHARS) setProblem(`${file.name || 'An image'} is over ${MAX_CHARS >> 20} MB after resizing.`);
        else added.push(img);
      } catch {
        setProblem(`${file.name || 'An image'} could not be read.`);
      }
    }
    setImages((prev) => [...prev, ...added].slice(0, MAX_IMAGES));
  };
  return {
    images,
    problem,
    add,
    remove: (i: number) => setImages((prev) => prev.filter((_, j) => j !== i)),
    clear: () => (setImages([]), setProblem('')),
    // Spread on the composer's form: pasting or dropping images attaches them.
    handlers: {
      onPaste: (e: ClipboardEvent) => {
        const files = [...e.clipboardData.files];
        if (files.some((f) => f.type.startsWith('image/'))) (e.preventDefault(), void add(files));
      },
      onDragOver: (e: DragEvent) => e.preventDefault(),
      onDrop: (e: DragEvent) => (e.preventDefault(), void add([...e.dataTransfer.files])),
    },
  };
}

export type Attachments = ReturnType<typeof useAttachments>;

/** The paperclip: opens the file picker, which on a phone offers the camera too. */
export function AttachButton({ attach }: { attach: Attachments }) {
  const input = useRef<HTMLInputElement>(null);
  const pick = (e: ChangeEvent<HTMLInputElement>) => {
    void attach.add([...(e.target.files ?? [])]);
    e.target.value = '';
  };
  return (
    <>
      <input ref={input} type="file" accept="image/*" multiple hidden onChange={pick} data-testid="attach-input" />
      <button type="button" aria-label="Attach images" onClick={() => input.current?.click()} className="grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-ink-2 hover:text-ink">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M21 12.5l-8.6 8.6a5.5 5.5 0 0 1-7.8-7.8l9-9a3.7 3.7 0 0 1 5.2 5.2l-9 9a1.8 1.8 0 0 1-2.6-2.6l8.2-8.2" /></svg>
      </button>
    </>
  );
}

/** Pending images before send, each with a remove button, and the problem with the last attempt. */
export function Pending({ attach }: { attach: Attachments }) {
  if (!attach.images.length && !attach.problem) return null;
  return (
    <div className="flex flex-col gap-1.5 px-1">
      {attach.problem && <p role="alert" className="m-0 text-[13px] text-danger">{attach.problem}</p>}
      <ul className="m-0 flex list-none flex-wrap gap-2 p-0">
        {attach.images.map((img, i) => (
          <li key={i} className="relative size-16 flex-none">
            <img src={dataUrl(img)} alt={`Attached image ${i + 1}`} className="size-16 rounded-xl object-cover ring-1 ring-line-strong" />
            <button type="button" aria-label={`Remove image ${i + 1}`} onClick={() => attach.remove(i)} className="absolute -top-3.5 -right-3.5 grid size-11 cursor-pointer place-items-center border-0 bg-transparent p-0">
              <span aria-hidden="true" className="grid size-6 place-items-center rounded-full bg-ink text-on-ink shadow-lift">
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round"><path d="M6 6l12 12M18 6L6 18" /></svg>
              </span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}

/** Thumbnails in the transcript; a tap opens the image at full size. */
export function Thumbs({ images }: { images: Img[] }) {
  const [open, setOpen] = useState<Img | null>(null);
  return (
    <>
      <ul className="m-0 mb-1 flex list-none flex-wrap justify-end gap-1.5 p-0">
        {images.map((img, i) => (
          <li key={i}>
            <button type="button" aria-label={`Open image ${i + 1}`} onClick={() => setOpen(img)} className="block cursor-zoom-in border-0 bg-transparent p-0">
              <img src={dataUrl(img)} alt={`Image ${i + 1}`} className="size-24 rounded-xl object-cover" />
            </button>
          </li>
        ))}
      </ul>
      {open && (
        <div role="dialog" aria-modal="true" aria-label="Image" onClick={() => setOpen(null)} onKeyDown={(e) => e.key === 'Escape' && setOpen(null)} className="fixed inset-0 z-50 grid place-items-center bg-black/80 p-4">
          <img src={dataUrl(open)} alt="Full size" className="max-h-full max-w-full object-contain" />
          <button type="button" autoFocus aria-label="Close image" onClick={() => setOpen(null)} className="absolute top-[max(0.5rem,env(safe-area-inset-top))] right-2 grid size-11 cursor-pointer place-items-center rounded-full border-0 bg-white/15 text-white">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg>
          </button>
        </div>
      )}
    </>
  );
}
