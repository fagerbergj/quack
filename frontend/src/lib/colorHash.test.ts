import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, it, expect } from 'vitest'
import { hashString, paletteTone } from './colorHash'
import { Badge } from '../components/ui/badge'

describe('hashString', () => {
  it('is deterministic', () => {
    expect(hashString('fagerbergj/quack')).toBe(hashString('fagerbergj/quack'))
  })

  it('differs for different inputs (not guaranteed, but true for these)', () => {
    expect(hashString('fagerbergj/quack')).not.toBe(hashString('fagerbergj/games'))
  })
})

describe('paletteTone', () => {
  it('returns the same classes for the same seed every time (#746 item 10/13)', () => {
    const a = paletteTone('fagerbergj/quack')
    const b = paletteTone('fagerbergj/quack')
    expect(a).toBe(b)
  })

  it('always names a Badge tone, so a hashed label gets a background with a text colour', () => {
    const tone = paletteTone('repo:NightsOut')
    expect(renderToStaticMarkup(createElement(Badge, { tone }, 'x'))).toMatch(/bg-\S+ text-\S+/)
  })
})
