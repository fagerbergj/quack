import { describe, it, expect } from 'vitest'
import { cn } from './utils'

describe('cn', () => {
  it('lets a later utility override an earlier one in the same group', () => {
    expect(cn('px-2 rounded-xl', 'px-4 rounded-2xl')).toBe('px-4 rounded-2xl')
  })

  // The theme's text-2xs is a font size; tailwind-merge would otherwise treat it as a colour and drop one.
  it('keeps text-2xs beside a text colour', () => {
    expect(cn('text-2xs', 'text-muted-foreground')).toBe('text-2xs text-muted-foreground')
  })
})
