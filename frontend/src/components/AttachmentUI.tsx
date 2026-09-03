import { useRef } from 'react'

export type AttachmentItem = { file: File; url: string }

export type AttachmentPreview = { url: string; mime: string; name: string }

// ImageThumbnail (#1138) is the one place an attached image renders: a
// bounded, object-fit thumbnail that opens the full-size image in a native
// <dialog> on click - shared by the composer's staged-file strip and the
// message-bubble preview so both look and behave the same way.
function ImageThumbnail({
  src, alt, className = 'h-16 w-16',
}: {
  src: string
  alt: string
  className?: string
}) {
  const dialogRef = useRef<HTMLDialogElement>(null)
  return (
    <>
      <button
        type="button"
        onClick={() => dialogRef.current?.showModal()}
        aria-label={`View ${alt} full size`}
        title={alt}
        className={`block rounded-lg overflow-hidden border border-gray-300 dark:border-gray-600 ${className}`}
      >
        <img src={src} alt={alt} className="h-full w-full object-cover" />
      </button>
      {/* Native <dialog> per the epic's dialogs-become-sheets guidance - full-
          viewport backdrop, Esc/backdrop-click close for free. */}
      <dialog
        ref={dialogRef}
        aria-label={alt}
        className="max-w-[92vw] max-h-[92vh] bg-transparent p-0 backdrop:bg-black/70"
        onClick={e => { if (e.target === dialogRef.current) dialogRef.current?.close() }}
      >
        <div className="relative">
          <img src={src} alt={alt} className="max-w-[92vw] max-h-[92vh] object-contain" />
          <button
            type="button"
            onClick={() => dialogRef.current?.close()}
            aria-label="Close"
            className="absolute top-2 right-2 min-w-[44px] min-h-[44px] flex items-center justify-center rounded-full bg-black/60 text-white text-xl"
          >
            ×
          </button>
        </div>
      </dialog>
    </>
  )
}

/** Attachments shown in the user message bubble: real thumbnails for images
    (click to view full size), a text chip for anything else. Always visible -
    no longer hidden behind a collapsed <details>, since an image the user
    can't see defeats the point (#1138). */
export function AttachmentPreviews({ previews }: { previews: AttachmentPreview[] }) {
  if (!previews.length) return null
  const images = previews.filter(p => p.mime.startsWith('image/'))
  const others = previews.filter(p => !p.mime.startsWith('image/'))
  return (
    <div className="mb-2 space-y-1.5">
      {images.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {images.map((p, i) => (
            <ImageThumbnail key={i} src={p.url} alt={p.name} />
          ))}
        </div>
      )}
      {others.length > 0 && (
        <ul className="space-y-0.5 text-xs text-blue-100 opacity-90">
          {others.map((p, i) => (
            <li key={i} className="truncate max-w-xs">
              🎵{' '}
              <a href={p.url} download={p.name} className="underline underline-offset-2 hover:opacity-100">
                {p.name}
              </a>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

/** Thumbnail strip rendered above the textarea while files are staged for send. */
export function AttachmentStrip({
  attachments,
  onRemove,
}: {
  attachments: AttachmentItem[]
  onRemove: (index: number) => void
}) {
  if (!attachments.length) return null
  return (
    <div className="flex flex-wrap gap-2">
      {attachments.map((a, i) => (
        <div key={i} className="relative group">
          {a.file.type.startsWith('image/') ? (
            <ImageThumbnail src={a.url} alt={a.file.name} className="h-16 w-16" />
          ) : (
            <div className="h-16 w-24 flex items-center justify-center rounded-lg border border-gray-300 dark:border-gray-600 bg-gray-100 dark:bg-gray-700 text-xs text-gray-500 dark:text-gray-400 px-1 text-center">
              {a.file.name}
            </div>
          )}
          <button
            type="button"
            onClick={() => onRemove(i)}
            className="absolute -top-1.5 -right-1.5 hidden group-hover:flex items-center justify-center w-5 h-5 rounded-full bg-gray-700 text-white text-xs"
            aria-label="Remove attachment"
          >
            ×
          </button>
        </div>
      ))}
    </div>
  )
}
