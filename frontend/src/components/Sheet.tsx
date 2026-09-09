import type { ReactNode } from 'react'
import { useDrawer } from '../hooks/useDrawer'

interface Props {
  onClose: () => void
  // Anchored: at medium+ the scrim collapses to `display: contents` so the
  // panel positions itself (medium:absolute …) inside its `relative` trigger
  // wrapper - a popover on desktop, a bottom sheet on a phone.
  anchored?: boolean
  role?: 'dialog' | 'menu'
  'aria-label'?: string
  className?: string
  children: ReactNode
}

// Sheet is the one modal shell for popups and menus (#1131 rule 6): below
// `medium` a bottom sheet over a scrim, padded past the composer's safe-area
// gap; at medium+ a centred dialog or an anchored popover. Escape, scrim
// click, focus trap and focus restore come from useDrawer.
export function Sheet({ onClose, anchored, role = 'dialog', className = '', children, ...aria }: Props) {
  const panelRef = useDrawer(true, onClose)
  return (
    <div
      className={`fixed inset-0 z-50 flex items-end justify-center bg-black/40 ${anchored ? 'medium:contents' : 'medium:items-center medium:p-4'}`}
      onClick={onClose}
    >
      <div
        ref={panelRef}
        role={role}
        aria-modal={role === 'dialog' || undefined}
        {...aria}
        className={`z-50 w-full max-h-[90dvh] overflow-y-auto rounded-t-2xl shadow-xl pb-[calc(0.75rem+var(--composer-gap))] ${className}`}
        onClick={e => e.stopPropagation()}
      >
        {children}
      </div>
    </div>
  )
}
