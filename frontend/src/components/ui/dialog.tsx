import { useLayoutEffect, useRef, type ComponentProps, type ReactNode, type RefObject } from 'react'
import * as DialogPrimitive from '@radix-ui/react-dialog'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '../../lib/utils'
import { lastMenuTrigger } from './dropdown-menu'

export const Dialog = DialogPrimitive.Root
export const DialogClose = DialogPrimitive.Close

// `responsive` is the app's dialog shape: a bottom sheet below `medium` (600px), a centred dialog from there up.
// `blocking` covers the whole viewport above every other layer (z-70) for the version-skew wall.
const contentVariants = cva('fixed flex flex-col bg-background text-foreground outline-none', {
  variants: {
    placement: {
      responsive: 'z-50 inset-x-0 bottom-0 max-h-[90dvh] rounded-t-2xl shadow-overlay pb-[calc(0.75rem+var(--composer-gap))] medium:inset-auto medium:left-1/2 medium:top-1/2 medium:max-h-[85dvh] medium:w-[calc(100%-2rem)] medium:max-w-2xl medium:-translate-x-1/2 medium:-translate-y-1/2 medium:rounded-2xl medium:pb-0',
      blocking: 'z-70 inset-0 items-center justify-center gap-3 bg-background/95 p-6 text-center text-sm',
    },
  },
  defaultVariants: { placement: 'responsive' },
})

function RememberOpener({ into }: { into: RefObject<HTMLElement | null> }) {
  useLayoutEffect(() => {
    const el = document.activeElement
    into.current = el instanceof HTMLElement && el !== document.body ? el : null
  }, [into])
  return null
}

// Radix returns focus only to a Dialog.Trigger, but these dialogs open from app state. `remember` must be the
// first child of the content (its layout effect runs before any autoFocus); closing then refocuses the opener.
// A menu item that opened the dialog unmounts with its menu, so a gone opener falls back to that menu's trigger.
export function useOpenerFocus(onCloseAutoFocus?: (e: Event) => void): { remember: ReactNode; onCloseAutoFocus: (e: Event) => void } {
  const opener = useRef<HTMLElement | null>(null)
  return {
    remember: <RememberOpener into={opener} />,
    onCloseAutoFocus: e => {
      onCloseAutoFocus?.(e)
      if (e.defaultPrevented) return
      e.preventDefault()
      const target = opener.current?.isConnected ? opener.current : lastMenuTrigger()
      target?.focus()
    },
  }
}

type ContentProps = ComponentProps<typeof DialogPrimitive.Content> & VariantProps<typeof contentVariants> & { title: string }

// `title` is the accessible name, rendered visually hidden: the screens keep their own visible headings.
// Radix reads a missing Description as a gap; these dialogs are named by their title alone.
export function DialogContent({ className, children, title, placement, onOpenAutoFocus, onCloseAutoFocus, ...props }: ContentProps) {
  const focus = useOpenerFocus(onCloseAutoFocus)
  return (
    <DialogPrimitive.Portal>
      <DialogPrimitive.Overlay data-slot="overlay" className={cn('fixed inset-0 bg-scrim', placement === 'blocking' ? 'z-70' : 'z-50')} />
      <DialogPrimitive.Content
        aria-describedby={undefined}
        onCloseAutoFocus={focus.onCloseAutoFocus}
        onOpenAutoFocus={e => {
          onOpenAutoFocus?.(e)
          if (e.defaultPrevented) return
          // A field marked data-autofocus wins; otherwise focus the dialog itself, not its first control, so a
          // tooltip-wrapped button is not announced and a single Escape closes.
          const root = e.currentTarget instanceof HTMLElement ? e.currentTarget : null
          e.preventDefault()
          ;(root?.querySelector<HTMLElement>('[data-autofocus]') ?? root)?.focus()
        }}
        className={cn(contentVariants({ placement }), className)}
        {...props}
      >
        {focus.remember}
        <DialogPrimitive.Title className="sr-only">{title}</DialogPrimitive.Title>
        {children}
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  )
}
