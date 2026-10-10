import { useEffect, useRef, type MouseEvent } from 'react'

const FOCUSABLE = 'button, a[href], input, select, textarea, [tabindex]:not([tabindex="-1"])'

// Shows the returned <dialog> while `open`: as a modal (Esc, focus trap, ::backdrop), or as a `popover="auto"`
// (Esc and light dismiss, page stays interactive). index.css already locks body scroll for the whole app.
export function useDrawer(open: boolean, popover = false) {
  const ref = useRef<HTMLDialogElement>(null)
  const opener = useRef<Element | null>(null)
  // The element last shown, so StrictMode's effect re-run neither re-shows it nor re-captures the opener.
  const shown = useRef<HTMLDialogElement | null>(null)

  useEffect(() => {
    const d = ref.current
    if (!d) return
    if (!open) {
      if (d.open) d.close()
      shown.current = null
      return
    }
    if (shown.current !== d) {
      shown.current = d
      opener.current = document.activeElement
      if (popover) d.showPopover()
      else d.showModal()
      // React's autoFocus fires while the dialog is still hidden, and a popover takes no initial focus itself.
      const first = d.querySelector<HTMLElement>('[data-autofocus]') ?? (popover ? d.querySelector<HTMLElement>(FOCUSABLE) : null)
      first?.focus()
    }
    // Removing an open dialog skips the native focus restore. Only reclaim focus nobody else took: a light
    // dismiss by clicking another control must leave focus on that control.
    return () => {
      const lost = !document.activeElement || document.activeElement === document.body
      if (lost && opener.current instanceof HTMLElement) opener.current.focus()
    }
  }, [open, popover])

  return ref
}

// A click whose target is the <dialog> itself landed on ::backdrop, given the panel fills the dialog box.
export function closeOnBackdrop(e: MouseEvent<HTMLDialogElement>) {
  if (e.target === e.currentTarget) e.currentTarget.close()
}
