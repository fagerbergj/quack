import { useState } from 'react'

// CopyButton is a small icon-style button that copies text to the clipboard,
// flashing a brief check mark for confirmation. It's the escape hatch every
// tool call carries (#404): whatever the rendered view above it does or
// doesn't show, the raw input/output JSON is always one click away - styled
// to match the codebase's other small icon buttons (ChatList's delete ×,
// AttachmentUI's remove ×): a bare glyph, muted, no border. The icon is the
// standard Material Design "content-copy" glyph (unambiguous at 12px, unlike
// a pencil) - a checkmark on success needs no icon library, so that one stays
// a plain glyph.
export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false)
  const copy = (e: React.MouseEvent) => {
    // Tool calls render inside a <details>/<summary>; a click on this button
    // must copy WITHOUT toggling the enclosing disclosure.
    e.preventDefault()
    e.stopPropagation()
    navigator.clipboard.writeText(text)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }
  return (
    <button
      type="button"
      onClick={copy}
      aria-label={copied ? `${label} - copied` : label}
      title={label}
      className="shrink-0 rounded p-0.5 leading-none text-gray-400 hover:text-gray-600 dark:text-gray-500 dark:hover:text-gray-300 transition-colors"
    >
      {copied ? (
        <span className="text-[11px]" aria-hidden="true">✓</span>
      ) : (
        <svg viewBox="0 0 24 24" width="12" height="12" fill="currentColor" aria-hidden="true">
          <path d="M19,21H8V7H19M19,5H8A2,2 0 0,0 6,7V21A2,2 0 0,0 8,23H19A2,2 0 0,0 21,21V7A2,2 0 0,0 19,5M16,1H4A2,2 0 0,0 2,3V17H4V3H16V1Z" />
        </svg>
      )}
    </button>
  )
}
