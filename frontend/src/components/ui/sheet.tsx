import type { ComponentProps } from 'react'
import * as DialogPrimitive from '@radix-ui/react-dialog'
import { cn } from '../../lib/utils'
import { useOpenerFocus } from './dialog'

export const Sheet = DialogPrimitive.Root

// An edge drawer (navigation, the phone chat list). Slides in from the left through a CSS transition on
// `starting:`; there is no exit animation, so Radix unmounts it the moment it closes.
export function SheetContent({ className, children, title, onCloseAutoFocus, onOpenAutoFocus, ...props }: ComponentProps<typeof DialogPrimitive.Content> & { title: string }) {
  const focus = useOpenerFocus(onCloseAutoFocus)
  return (
    <DialogPrimitive.Portal>
      <DialogPrimitive.Overlay data-slot="overlay" className="fixed inset-0 z-50 bg-scrim" />
      <DialogPrimitive.Content
        aria-describedby={undefined}
        onCloseAutoFocus={focus.onCloseAutoFocus}
        onOpenAutoFocus={e => {
          onOpenAutoFocus?.(e)
          if (e.defaultPrevented) return
          // Focus the drawer itself, not its first control (see DialogContent).
          e.preventDefault()
          if (e.currentTarget instanceof HTMLElement) e.currentTarget.focus()
        }}
        className={cn('fixed inset-y-0 left-0 z-50 flex h-dvh w-64 max-w-[85vw] flex-col border-r border-border bg-card text-card-foreground shadow-overlay outline-none transition-transform duration-200 starting:-translate-x-full', className)}
        {...props}
      >
        {focus.remember}
        <DialogPrimitive.Title className="sr-only">{title}</DialogPrimitive.Title>
        {children}
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  )
}
