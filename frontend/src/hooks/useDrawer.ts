import { useEffect, useRef } from 'react'

// Off-canvas drawer a11y: Esc closes, focus is trapped in the panel while open and returns
// to the opener on close. index.css already locks body scroll for the whole app.
export function useDrawer(open: boolean, onClose: () => void) {
  const panelRef = useRef<HTMLDivElement>(null)
  // Callers pass inline closures; a ref keeps the effect from re-running and re-stealing focus.
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

  useEffect(() => {
    if (!open) return
    const opener = document.activeElement
    const panel = panelRef.current
    function focusable(): HTMLElement[] {
      return Array.from(panel?.querySelectorAll<HTMLElement>('button, a[href], input, [tabindex]:not([tabindex="-1"])') ?? [])
    }
    // A panel that autofocuses its own input (NodePopup's answer box) keeps it.
    if (!panel?.contains(document.activeElement)) focusable()[0]?.focus()

    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.stopPropagation()
        onCloseRef.current()
        return
      }
      if (e.key !== 'Tab') return
      const items = focusable()
      if (items.length === 0) return
      const first = items[0]
      const last = items[items.length - 1]
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      if (opener instanceof HTMLElement) opener.focus()
    }
  }, [open])

  return panelRef
}
