import { useEffect, useRef } from 'react'

// useDrawer wires the a11y behavior every off-canvas drawer needs (#1131,
// MDN's dialogs-become-sheets guidance): Esc closes, focus moves into the
// panel and is trapped there while open, background scroll is locked, and
// focus returns to whatever opened it on close. Shared by NavRail's nav
// drawer and ChatList's mobile drawer so both off-canvas panels behave
// identically, not just look identical.
export function useDrawer(open: boolean, onClose: () => void) {
  const panelRef = useRef<HTMLDivElement>(null)
  // Both call sites pass an inline closure, so it gets a new identity on
  // every render of the owning component - a ref keeps the effect below
  // from tearing down/re-running (and re-stealing focus) on every one of
  // those re-renders (e.g. Chat's 5s chat-list poll) while the drawer is open.
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

  useEffect(() => {
    if (!open) return
    const opener = document.activeElement
    const panel = panelRef.current
    function focusable(): HTMLElement[] {
      return Array.from(panel?.querySelectorAll<HTMLElement>('button, a[href], input, [tabindex]:not([tabindex="-1"])') ?? [])
    }
    focusable()[0]?.focus()

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
    const prevOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      document.body.style.overflow = prevOverflow
      if (opener instanceof HTMLElement) opener.focus()
    }
  }, [open])

  return panelRef
}
