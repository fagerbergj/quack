import type { ComponentProps } from 'react'
import { Slot } from '@radix-ui/react-slot'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '../../lib/utils'

// The one in-page surface. `default` is a hairline-bordered card on the page background; `flat` is a quiet
// filled well for content nested inside a card, so nesting never stacks a second border. Elevation is reserved
// for floating layers (menus, popovers, dialogs); a Card never carries a shadow.
const cardVariants = cva('text-card-foreground', {
  variants: {
    variant: {
      default: 'rounded-xl border border-border bg-card',
      flat: 'rounded-lg bg-muted',
    },
  },
  defaultVariants: { variant: 'default' },
})

type Props = ComponentProps<'div'> & VariantProps<typeof cardVariants> & { asChild?: boolean }

export function Card({ className, variant, asChild, ...props }: Props) {
  const Comp = asChild ? Slot : 'div'
  return <Comp className={cn(cardVariants({ variant }), className)} {...props} />
}
