import { useState } from 'react'
import { Icon } from './Icon'
import { Button } from './ui/button'
import { Tooltip } from './ui/tooltip'

// The escape hatch every tool call carries: whatever the rendered view shows, the raw JSON is one click
// away. Uses "content-copy" because it stays unambiguous at 12px.
export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false)
  const copy = (e: React.MouseEvent) => {
    // Tool calls render inside a <details>/<summary>; a click on this button
    // must copy WITHOUT toggling the enclosing disclosure.
    e.preventDefault()
    e.stopPropagation()
    void navigator.clipboard.writeText(text)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }
  return (
    <Tooltip content={label}>
      <Button
        variant="ghost"
        size="icon"
        onClick={copy}
        aria-label={copied ? `${label} - copied` : label}
        className="medium:size-7"
      >
        {copied ? (
          <Icon name="check" className="w-3 h-3" />
        ) : (
          <svg viewBox="0 0 24 24" width="12" height="12" fill="currentColor" aria-hidden="true">
            <path d="M19,21H8V7H19M19,5H8A2,2 0 0,0 6,7V21A2,2 0 0,0 8,23H19A2,2 0 0,0 21,21V7A2,2 0 0,0 19,5M16,1H4A2,2 0 0,0 2,3V17H4V3H16V1Z" />
          </svg>
        )}
      </Button>
    </Tooltip>
  )
}
