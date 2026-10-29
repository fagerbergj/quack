import type { ComponentProps } from 'react'
import * as DialogPrimitive from '@radix-ui/react-dialog'
import { cn } from '../../lib/utils'

export const Sheet = DialogPrimitive.Root

// An edge drawer (navigation, the phone chat list). Slides in from the left through a CSS transition on
// `starting:`; there is no exit animation, so Radix unmounts it the moment it closes.
export function SheetContent({ className, children, title, ...props }: ComponentProps<typeof DialogPrimitive.Content> & { title: string }) {
  return (
    <DialogPrimitive.Portal>
      <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-scrim" />
      <DialogPrimitive.Content
        aria-describedby={undefined}
        className={cn('fixed inset-y-0 left-0 z-50 flex h-dvh w-64 max-w-[85vw] flex-col border-r border-border bg-card text-card-foreground shadow-overlay outline-none transition-transform duration-200 starting:-translate-x-full', className)}
        {...props}
      >
        <DialogPrimitive.Title className="sr-only">{title}</DialogPrimitive.Title>
        {children}
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  )
}
