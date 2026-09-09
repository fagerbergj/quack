// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { NodePopup } from './NodePopup'
import type { DagNodeDef } from '../state/agentStream'

afterEach(cleanup)

const node: DagNodeDef = { id: 'r1', agent: 'web-researcher', task: 'Research Dublin.', depends_on: [] }

// Mobile QA (390px): the popup is a bottom sheet below the medium breakpoint
// and its close control is a 44px target - the 28px one missed on a phone.
describe('NodePopup compact sheet', () => {
  it('close is a 44px target', () => {
    render(<NodePopup node={node} state={{ status: 'done' }} onClose={() => {}} />)
    const close = screen.getByRole('button', { name: 'Close' })
    expect(close.className).toContain('h-11')
    expect(close.className).toContain('w-11')
  })

  it('returns focus to the control that opened it when it closes', () => {
    const opener = document.createElement('button')
    document.body.appendChild(opener)
    opener.focus()
    const { unmount } = render(<NodePopup node={node} state={{ status: 'done' }} onClose={() => {}} />)
    unmount()
    expect(document.activeElement).toBe(opener)
    opener.remove()
  })

  it('docks to the bottom edge below medium and centers above it', () => {
    render(<NodePopup node={node} state={{ status: 'done' }} onClose={() => {}} />)
    const panel = screen.getByRole('dialog')
    expect(panel.parentElement?.className).toContain('items-end')
    expect(panel.parentElement?.className).toContain('medium:items-center')
    expect(panel.className).toContain('rounded-t-2xl')
    expect(panel.className).toContain('medium:rounded-2xl')
  })
})
