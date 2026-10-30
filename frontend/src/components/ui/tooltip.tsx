import type { ReactElement, ReactNode } from 'react'
import * as TooltipPrimitive from '@radix-ui/react-tooltip'

// Each Tooltip carries its own provider so no app-level wrapper is needed. Hover and keyboard focus only: a
// tooltip never holds the sole name of a control (keep an aria-label on icon buttons), since touch has no hover.
// Programmatic focus (a dialog or menu handing focus back) does not open it unless :focus-visible matches, so it
// neither shows on a phone nor takes the first Escape.
function focusIsVisible(el: Element): boolean {
  try { return el.matches(':focus-visible') } catch { return true }
}

export function Tooltip({ content, children, side = 'top' }: { content: ReactNode; children: ReactElement; side?: 'top' | 'bottom' | 'left' | 'right' }) {
  return (
    <TooltipPrimitive.Provider delayDuration={400}>
      <TooltipPrimitive.Root>
        <TooltipPrimitive.Trigger asChild onFocus={e => { if (!focusIsVisible(e.currentTarget)) e.preventDefault() }}>
          {children}
        </TooltipPrimitive.Trigger>
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
