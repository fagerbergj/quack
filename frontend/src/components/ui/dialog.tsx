import type { ComponentProps } from 'react'
import * as DialogPrimitive from '@radix-ui/react-dialog'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '../../lib/utils'

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

type ContentProps = ComponentProps<typeof DialogPrimitive.Content> & VariantProps<typeof contentVariants> & { title: string }

// `title` is the accessible name, rendered visually hidden: the screens keep their own visible headings.
// Radix reads a missing Description as a gap; these dialogs are named by their title alone.
export function DialogContent({ className, children, title, placement, onOpenAutoFocus, ...props }: ContentProps) {
  return (
    <DialogPrimitive.Portal>
      <DialogPrimitive.Overlay className={cn('fixed inset-0 bg-scrim', placement === 'blocking' ? 'z-70' : 'z-50')} />
      <DialogPrimitive.Content
        aria-describedby={undefined}
        onOpenAutoFocus={e => {
          onOpenAutoFocus?.(e)
          if (e.defaultPrevented) return
          // A field marked data-autofocus wins over Radix's first-tabbable default.
          const target = e.currentTarget instanceof HTMLElement ? e.currentTarget.querySelector<HTMLElement>('[data-autofocus]') : null
          if (target) { e.preventDefault(); target.focus() }
        }}
        className={cn(contentVariants({ placement }), className)}
        {...props}
      >
        <DialogPrimitive.Title className="sr-only">{title}</DialogPrimitive.Title>
        {children}
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  )
}
