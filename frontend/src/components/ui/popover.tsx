import type { ComponentProps } from 'react'
import * as PopoverPrimitive from '@radix-ui/react-popover'
import { cn } from '../../lib/utils'

export const Popover = PopoverPrimitive.Root
export const PopoverTrigger = PopoverPrimitive.Trigger

// z-60 keeps it above a Dialog (z-50) it may be opened from.
export function PopoverContent({ className, align = 'start', sideOffset = 4, ...props }: ComponentProps<typeof PopoverPrimitive.Content>) {
  return (
    <PopoverPrimitive.Portal>
      <PopoverPrimitive.Content
        align={align}
        sideOffset={sideOffset}
        collisionPadding={8}
        className={cn('z-60 max-h-(--radix-popover-content-available-height) max-w-[calc(100vw-1rem)] overflow-y-auto rounded-xl border border-border bg-popover p-2 text-sm text-popover-foreground shadow-overlay outline-none medium:text-xs', className)}
        {...props}
      />
    </PopoverPrimitive.Portal>
  )
}
