import type { ComponentProps } from 'react'
import * as MenuPrimitive from '@radix-ui/react-dropdown-menu'
import { cn } from '../../lib/utils'

export const DropdownMenu = MenuPrimitive.Root
export const DropdownMenuTrigger = MenuPrimitive.Trigger
export const DropdownMenuRadioGroup = MenuPrimitive.RadioGroup

// z-60 keeps it above a Dialog (z-50) it may be opened from. Items are 44px on compact, denser from `medium`.
export function DropdownMenuContent({ className, align = 'end', sideOffset = 4, ...props }: ComponentProps<typeof MenuPrimitive.Content>) {
  return (
    <MenuPrimitive.Portal>
      <MenuPrimitive.Content
        align={align}
        sideOffset={sideOffset}
        collisionPadding={8}
        className={cn('z-60 min-w-40 max-w-[calc(100vw-1rem)] overflow-y-auto rounded-xl border border-border bg-popover p-1 text-sm text-popover-foreground shadow-overlay medium:text-xs', className)}
        {...props}
      />
    </MenuPrimitive.Portal>
  )
}

const ITEM =
  'relative flex min-h-11 cursor-default select-none items-center gap-2 rounded-md px-3 py-2 outline-none data-[highlighted]:bg-accent data-[highlighted]:text-accent-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50 medium:min-h-8 medium:py-1.5'

export function DropdownMenuItem({ className, destructive, ...props }: ComponentProps<typeof MenuPrimitive.Item> & { destructive?: boolean }) {
  return <MenuPrimitive.Item className={cn(ITEM, destructive && 'text-destructive data-[highlighted]:text-destructive', className)} {...props} />
}

// The checked state is the item's aria-checked; the dot is decoration for sighted users.
export function DropdownMenuRadioItem({ className, children, ...props }: ComponentProps<typeof MenuPrimitive.RadioItem>) {
  return (
    <MenuPrimitive.RadioItem className={cn(ITEM, className)} {...props}>
      <span aria-hidden="true" className="inline-flex w-3 justify-center">
        <MenuPrimitive.ItemIndicator>
          <span className="block size-1.5 rounded-full bg-current" />
        </MenuPrimitive.ItemIndicator>
      </span>
      {children}
    </MenuPrimitive.RadioItem>
  )
}

export function DropdownMenuSeparator({ className, ...props }: ComponentProps<typeof MenuPrimitive.Separator>) {
  return <MenuPrimitive.Separator className={cn('-mx-1 my-1 h-px bg-border', className)} {...props} />
}
