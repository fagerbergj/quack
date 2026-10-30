import type { ComponentProps } from 'react'
import { cn } from '../../lib/utils'

// 16px text on compact: iOS Safari zooms the page when a focused field is smaller. --input is the 3:1 border
// colour (WCAG 1.4.11), so an input stays findable without a heavier fill.
const FIELD =
  'w-full min-w-0 rounded-md border border-input bg-card px-3 text-base text-foreground medium:text-sm placeholder:text-muted-foreground transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive'

export function Input({ className, ...props }: ComponentProps<'input'>) {
  return <input className={cn(FIELD, 'h-11 medium:h-9', className)} {...props} />
}

export function Textarea({ className, ...props }: ComponentProps<'textarea'>) {
  return <textarea className={cn(FIELD, 'min-h-11 py-2', className)} {...props} />
}
