import { useState } from 'react'
import { Icon } from './Icon'
import { Button } from './ui/button'
import { Dialog, DialogClose, DialogContent } from './ui/dialog'

export type AttachmentItem = { file: File; url: string }

export type AttachmentPreview = { url: string; mime: string; name: string }

// The one place an attached image renders, shared by the composer's staged strip and the message bubble
// so both behave the same; click opens full size in a dialog.
function ImageThumbnail({
  src, alt, className = 'h-16 w-16',
}: {
  src: string
  alt: string
  className?: string
}) {
  const [open, setOpen] = useState(false)
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      {/* size={null}: a thumbnail's box comes from the caller, not a button size. */}
      <Button
        variant="outline"
        size={null}
        onClick={() => setOpen(true)}
        aria-label={`View ${alt} full size`}
        title={alt}
        className={`block overflow-hidden p-0 ${className}`}
      >
        <img src={src} alt={alt} className="h-full w-full object-cover" />
      </Button>
      <DialogContent
        title={alt}
        className="inset-auto left-1/2 top-1/2 w-auto max-w-[92vw] -translate-x-1/2 -translate-y-1/2 rounded-2xl bg-transparent pb-0 shadow-none medium:max-w-[92vw]"
      >
        <img src={src} alt={alt} className="max-w-[92vw] max-h-[92dvh] object-contain" />
        <DialogClose asChild>
          <Button variant="ghost" size="icon" aria-label="Close" className="absolute right-2 top-2 rounded-full bg-black/60 text-xl text-white hover:bg-black/80 hover:text-white">
            ×
          </Button>
        </DialogClose>
      </DialogContent>
    </Dialog>
  )
}

/** Always visible rather than behind a collapsed <details>: an image the user can't see defeats the point. */
export function AttachmentPreviews({ previews }: { previews: AttachmentPreview[] }) {
  if (!previews.length) return null
  const images = previews.filter(p => p.mime.startsWith('image/'))
  const others = previews.filter(p => !p.mime.startsWith('image/'))
  return (
    <div className="mb-2 space-y-2">
      {images.length > 0 && (
        <div className="flex flex-wrap gap-2">
          {images.map((p, i) => (
            <ImageThumbnail key={i} src={p.url} alt={p.name} />
          ))}
        </div>
      )}
      {others.length > 0 && (
        <ul className="space-y-0.5 text-xs text-blue-100 opacity-90">
          {others.map((p, i) => (
            <li key={i} className="flex items-center gap-1 truncate max-w-xs">
              <Icon name="music" className="w-3.5 h-3.5 shrink-0" />
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
            <div className="h-16 w-24 flex items-center justify-center rounded-lg border border-gray-300 dark:border-gray-600 bg-gray-100 dark:bg-gray-700 text-xs text-gray-500 dark:text-gray-300 px-1 text-center">
              {a.file.name}
            </div>
          )}
          <Button
            variant="secondary"
            size={null}
            onClick={() => onRemove(i)}
            className="absolute -top-1.5 -right-1.5 hidden size-5 rounded-full bg-gray-700 text-xs text-white hover:bg-gray-600 focus-visible:flex group-hover:flex"
            aria-label="Remove attachment"
          >
            ×
          </Button>
        </div>
      ))}
    </div>
  )
}
