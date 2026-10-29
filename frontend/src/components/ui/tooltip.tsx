import type { ReactElement, ReactNode } from 'react'
import * as TooltipPrimitive from '@radix-ui/react-tooltip'

// Each Tooltip carries its own provider so no app-level wrapper is needed. Hover and keyboard focus only: a
// tooltip never holds the sole name of a control (keep an aria-label on icon buttons), since touch has no hover.
export function Tooltip({ content, children, side = 'top' }: { content: ReactNode; children: ReactElement; side?: 'top' | 'bottom' | 'left' | 'right' }) {
  return (
    <TooltipPrimitive.Provider delayDuration={400}>
      <TooltipPrimitive.Root>
        <TooltipPrimitive.Trigger asChild>{children}</TooltipPrimitive.Trigger>
        <TooltipPrimitive.Portal>
          <TooltipPrimitive.Content
            side={side}
            sideOffset={4}
            className="z-60 max-w-xs rounded-md bg-foreground px-2 py-1 text-xs text-background"
          >
            {content}
          </TooltipPrimitive.Content>
        </TooltipPrimitive.Portal>
      </TooltipPrimitive.Root>
    </TooltipPrimitive.Provider>
  )
}
