import { useEffect, useId, type ReactNode } from 'react'
import { closeOnBackdrop, useDrawer } from '../hooks/useDrawer'
import { useMediaQuery } from '../hooks/useMediaQuery'

interface Props {
  onClose: () => void
  // At medium+ a non-modal popover anchored below its parent element (the trigger wrapper), lining up the
  // named edge; below medium the same modal bottom sheet as the unanchored mode.
  anchored?: 'left' | 'right'
  role?: 'dialog' | 'menu'
  'aria-label': string
  className?: string
  children: ReactNode
}

const SHELL = {
  centered: 'medium:m-auto medium:w-[calc(100%-2rem)] medium:max-w-2xl',
  left: 'medium:m-0 medium:mt-1 medium:inset-auto medium:w-fit medium:max-w-none medium:backdrop:bg-transparent medium:[position-area:bottom_span-right]',
  right: 'medium:m-0 medium:mt-1 medium:inset-auto medium:w-fit medium:max-w-none medium:backdrop:bg-transparent medium:[position-area:bottom_span-left]',
}

// The one shell for popups and menus: a modal bottom sheet below `medium`, a modal dialog or anchored popover
// above. Native <dialog>/popover supply Esc and dismissal; useDrawer restores focus on unmount.
export function Sheet({ onClose, anchored, role = 'dialog', className = '', children, 'aria-label': label }: Props) {
  // Must match the `medium` (600px) breakpoint the SHELL classes switch on.
  const compact = useMediaQuery('(max-width: 599px)')
  const popover = !!anchored && !compact
  const dialogRef = useDrawer(true, popover)
  const anchor = `--sheet${useId().replace(/\W/g, '')}`

  // The top layer can't be `absolute` to the trigger wrapper, so anchor to it instead.
  useEffect(() => {
    const parent = dialogRef.current?.parentElement
    if (!anchored || !parent) return
    parent.style.setProperty('anchor-name', anchor)
    return () => { parent.style.removeProperty('anchor-name') }
  }, [anchored, anchor, dialogRef])

  return (
    <dialog
      // Remount on a breakpoint change: an open element can't switch between modal and popover.
      key={popover ? 'popover' : 'modal'}
      ref={dialogRef}
      popover={popover ? 'auto' : undefined}
      aria-label={label}
      onClose={onClose}
      onToggle={e => { if (e.newState === 'closed') onClose() }}
      onClick={closeOnBackdrop}
      style={anchored ? { positionAnchor: anchor } : undefined}
      className={`m-0 mt-auto w-full max-w-full max-h-[90dvh] p-0 border-0 bg-transparent backdrop:bg-black/40 ${SHELL[anchored ?? 'centered']}`}
    >
      <div
        role={role === 'menu' ? 'menu' : undefined}
        className={`w-full max-h-[90dvh] overflow-y-auto rounded-t-2xl shadow-xl pb-[calc(0.75rem+var(--composer-gap))] ${className}`}
      >
        {children}
      </div>
    </dialog>
  )
}
