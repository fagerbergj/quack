import type { ComponentProps } from 'react'
import { Slot } from '@radix-ui/react-slot'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '../../lib/utils'

// Sizes are 44px on compact and denser from `medium` up (WCAG 2.2 / M3 touch targets); `inline` keeps the 44px
// tap area on a phone but adds no height to the surrounding text on desktop.
const buttonVariants = cva(
  'inline-flex shrink-0 items-center justify-center gap-2 whitespace-nowrap rounded-md font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background disabled:pointer-events-none disabled:opacity-50 aria-disabled:pointer-events-none aria-disabled:opacity-50',
  {
    variants: {
      variant: {
        default: 'bg-primary text-primary-foreground hover:bg-primary/90',
        destructive: 'bg-destructive text-destructive-foreground hover:bg-destructive/90',
        outline: 'border border-input bg-card text-foreground hover:bg-accent',
        secondary: 'bg-muted text-foreground hover:bg-accent',
        ghost: 'text-muted-foreground hover:bg-accent hover:text-accent-foreground',
        link: 'rounded-sm text-primary underline-offset-4 hover:underline',
      },
      size: {
        default: 'h-11 px-4 text-sm medium:h-9',
        sm: 'h-11 px-3 text-xs medium:h-8',
        icon: 'size-11 medium:size-9',
        inline: 'min-h-11 text-xs medium:min-h-6',
      },
    },
    defaultVariants: { variant: 'default', size: 'default' },
  },
)

type Props = ComponentProps<'button'> & VariantProps<typeof buttonVariants> & { asChild?: boolean }

export function Button({ className, variant, size, asChild, type = 'button', ...props }: Props) {
  const Comp = asChild ? Slot : 'button'
  return <Comp {...(asChild ? {} : { type })} className={cn(buttonVariants({ variant, size }), className)} {...props} />
}
