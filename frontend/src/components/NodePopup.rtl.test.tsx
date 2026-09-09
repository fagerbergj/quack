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

  it('docks to the bottom edge below medium and centers above it', () => {
    render(<NodePopup node={node} state={{ status: 'done' }} onClose={() => {}} />)
    const dialog = screen.getByRole('dialog')
    expect(dialog.className).toContain('items-end')
    expect(dialog.className).toContain('medium:items-center')
    const panel = dialog.firstElementChild as HTMLElement
    expect(panel.className).toContain('rounded-t-2xl')
    expect(panel.className).toContain('medium:rounded-2xl')
  })
})
