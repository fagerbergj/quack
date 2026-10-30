import type { ComponentProps } from 'react'
import { Slot } from '@radix-ui/react-slot'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '../../lib/utils'

// The one chip shape (M3 label-small, 4dp corners, 24px tall so a linked chip meets WCAG 2.2 target size).
// Each tone pairs light/dark fills with a same-hue text colour; colour never carries meaning alone, so
// callers always pair a tone with its text or an icon.
const TONES = {
  gray: 'bg-muted text-muted-foreground',
  green: 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400',
  red: 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400',
  yellow: 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-500',
  amber: 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300',
  blue: 'bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-400',
  orange: 'bg-orange-100 text-orange-700 dark:bg-orange-900/40 dark:text-orange-300',
  lime: 'bg-lime-100 text-lime-700 dark:bg-lime-900/40 dark:text-lime-300',
  emerald: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300',
  teal: 'bg-teal-100 text-teal-700 dark:bg-teal-900/40 dark:text-teal-300',
  cyan: 'bg-cyan-100 text-cyan-700 dark:bg-cyan-900/40 dark:text-cyan-300',
  indigo: 'bg-indigo-100 text-indigo-700 dark:bg-indigo-900/40 dark:text-indigo-300',
  purple: 'bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-400',
  pink: 'bg-pink-100 text-pink-700 dark:bg-pink-900/40 dark:text-pink-300',
} as const

export type BadgeTone = keyof typeof TONES

const badgeVariants = cva('inline-flex shrink-0 items-center gap-1 rounded-sm px-2 py-1 text-2xs font-medium', {
  variants: { tone: TONES },
  defaultVariants: { tone: 'gray' },
})

type Props = ComponentProps<'span'> & VariantProps<typeof badgeVariants> & { asChild?: boolean }

// asChild styles a link as a chip while keeping its href and semantics.
export function Badge({ className, tone, asChild, ...props }: Props) {
  const Comp = asChild ? Slot : 'span'
  return <Comp className={cn(badgeVariants({ tone }), className)} {...props} />
}
