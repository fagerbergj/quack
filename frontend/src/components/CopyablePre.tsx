import { useRef, useState } from 'react'
import type { ComponentPropsWithoutRef } from 'react'

// CopyablePre wraps a fenced code block in a relative container with a one-click
// copy button. rehype-highlight runs AFTER sanitize (AgentParts' AssistantText),
// so the hljs token markup is intact by the time this renders. Also reused as
// the fallback rendering for a mermaid block that's invalid or still streaming
// (MermaidDiagram / AgentParts) - a plain code block is always a safe fallback.
export function CopyablePre({ children, ...props }: ComponentPropsWithoutRef<'pre'>) {
  const ref = useRef<HTMLPreElement>(null)
  const [copied, setCopied] = useState(false)
  const copy = () => {
    navigator.clipboard.writeText(ref.current?.textContent ?? '')
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }
  return (
    <div className="relative group not-prose">
      <button
        type="button"
        onClick={copy}
        aria-label="Copy code"
        className="absolute right-2 top-2 z-10 rounded border border-gray-600 bg-gray-800/80 px-2 py-0.5 text-[11px] text-gray-300 opacity-0 group-hover:opacity-100 focus:opacity-100 transition-opacity hover:bg-gray-700"
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
      <pre ref={ref} {...props}>{children}</pre>
    </div>
  )
}
