import { useState } from 'react'

// CopyButton is a small icon-style button that copies text to the clipboard,
// flashing a brief check mark for confirmation. It's the escape hatch every
// tool call carries (#404): whatever the rendered view above it does or
// doesn't show, the raw input/output JSON is always one click away — styled
// to match the codebase's other small icon buttons (ChatList's delete ×,
// AttachmentUI's remove ×): a bare glyph, muted, no border.
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
      aria-label={copied ? `${label} — copied` : label}
      title={label}
      className="shrink-0 rounded px-1 py-0.5 text-[11px] leading-none text-gray-400 hover:text-gray-600 dark:text-gray-500 dark:hover:text-gray-300 transition-colors"
    >
      {copied ? '✓' : '✎'}
    </button>
  )
}
