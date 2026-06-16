export type AttachmentItem = { file: File; url: string }

export type AttachmentPreview = { url: string; mime: string; name: string }

/** Compact collapsible chip shown in the user message bubble. */
export function AttachmentPreviews({ previews }: { previews: AttachmentPreview[] }) {
  if (!previews.length) return null
  const label = previews.length === 1 ? '1 attachment' : `${previews.length} attachments`
  return (
    <details className="mb-2 text-xs text-blue-100">
      <summary className="cursor-pointer select-none opacity-80 hover:opacity-100">
        📎 {label}
      </summary>
      <ul className="mt-1 ml-4 space-y-0.5 opacity-90">
        {previews.map((p, i) => (
          <li key={i} className="truncate max-w-xs">
            {p.mime.startsWith('image/') ? '🖼' : '🎵'}{' '}
            <a href={p.url} download={p.name} className="underline underline-offset-2 hover:opacity-100">
              {p.name}
            </a>
          </li>
        ))}
      </ul>
    </details>
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
            <img src={a.url} alt={a.file.name}
              className="h-16 w-16 object-cover rounded-lg border border-gray-300 dark:border-gray-600" />
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
