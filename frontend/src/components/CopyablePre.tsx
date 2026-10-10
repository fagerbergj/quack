import { useRef, useState } from 'react'
import type { ComponentPropsWithoutRef } from 'react'

// rehype-highlight runs after sanitize, so hljs markup is intact here. Also the fallback for a mermaid
// block that's invalid or still streaming.
export function CopyablePre({ children, ...props }: ComponentPropsWithoutRef<'pre'>) {
  const ref = useRef<HTMLPreElement>(null)
  const [copied, setCopied] = useState(false)
  const copy = () => {
    void navigator.clipboard.writeText(ref.current?.textContent ?? '')
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }
  return (
    <div className="relative group not-prose">
      <button
        type="button"
        onClick={copy}
        aria-label="Copy code"
        className="absolute right-2 top-2 z-10 min-h-[44px] min-w-[44px] inline-flex items-center justify-center rounded border border-gray-600 bg-gray-800/80 px-2 text-[11px] text-gray-300 opacity-0 group-hover:opacity-100 focus:opacity-100 transition-opacity hover:bg-gray-700"
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
      <pre ref={ref} {...props}>{children}</pre>
    </div>
  )
}
