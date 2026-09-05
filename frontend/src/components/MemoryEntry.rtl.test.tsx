// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryEntry } from './MemoryEntry'
import type { Memory } from '../api'

afterEach(cleanup)

const memory: Memory = {
  id: 'm1', content: 'A fact', bucket: 'repo:quack', author: 'agent', timestamp: new Date().toISOString(), kind: 'fact',
}

// #1137: the Forget button measured 28x28px (w-7 h-7) - under the 44px
// comfortable touch target. min-w/min-h fixes the hit area without growing
// the ✕ glyph itself.
describe('MemoryEntry Forget touch target (#1137)', () => {
  it('is a 44x44 tap area', () => {
    render(<MemoryEntry memory={memory} onForget={async () => {}} />)
    const btn = screen.getByRole('button', { name: /Forget:/ })
    expect(btn.className).toContain('min-w-[44px]')
    expect(btn.className).toContain('min-h-[44px]')
  })
})
