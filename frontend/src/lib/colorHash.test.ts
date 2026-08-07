import { describe, it, expect } from 'vitest'
import { hashString, paletteClasses } from './colorHash'

describe('hashString', () => {
  it('is deterministic', () => {
    expect(hashString('fagerbergj/quack')).toBe(hashString('fagerbergj/quack'))
  })

  it('differs for different inputs (not guaranteed, but true for these)', () => {
    expect(hashString('fagerbergj/quack')).not.toBe(hashString('fagerbergj/games'))
  })
})

describe('paletteClasses', () => {
  it('returns the same classes for the same seed every time (#746 item 10/13)', () => {
    const a = paletteClasses('fagerbergj/quack')
    const b = paletteClasses('fagerbergj/quack')
    expect(a).toBe(b)
  })

  it('always pairs a background with a text colour', () => {
    expect(paletteClasses('repo:NightsOut')).toMatch(/^bg-\S+ text-\S+ dark:bg-\S+ dark:text-\S+$/)
  })
})
