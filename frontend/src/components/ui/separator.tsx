import type { ComponentProps } from 'react'
import { cn } from '../../lib/utils'

// A hairline between groups: decorative, so screen readers skip it.
export function Separator({ className, orientation = 'horizontal', ...props }: ComponentProps<'div'> & { orientation?: 'horizontal' | 'vertical' }) {
  return (
    <div
      aria-hidden="true"
      data-orientation={orientation}
      className={cn('shrink-0 bg-border', orientation === 'horizontal' ? 'h-px w-full' : 'h-full w-px', className)}
      {...props}
    />
  )
}
