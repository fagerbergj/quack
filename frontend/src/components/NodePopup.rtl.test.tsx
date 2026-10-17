// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
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
    const dialog = screen.getByRole('dialog', { name: 'Node web-researcher' })
    expect(dialog.className).toContain('mt-auto')
    expect(dialog.className).toContain('medium:m-auto')
    const panel = dialog.firstElementChild!
    expect(panel.className).toContain('rounded-t-2xl')
    expect(panel.className).toContain('medium:rounded-2xl')
  })
})

describe('NodePopup inline editors', () => {
  const queued = { id: 'q1', text: 'Also check rainfall.', status: 'queued' as const, delivered: false, created_at: '2026-10-17T00:00:00Z' }

  it('saves an edited queued message and drops a cancelled edit', async () => {
    const user = userEvent.setup()
    const onEdit = vi.fn()
    render(<NodePopup node={node} state={{ status: 'running', queue: [queued] }} onClose={() => {}} onEditQueuedMessage={onEdit} />)
    await user.click(screen.getByRole('button', { name: 'Edit' }))
    await user.type(screen.getByDisplayValue('Also check rainfall.'), ' Winter only.')
    await user.click(screen.getByRole('button', { name: 'cancel' }))
    expect(onEdit).not.toHaveBeenCalled()
    expect(screen.getByText('Also check rainfall.')).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Edit' }))
    await user.type(screen.getByDisplayValue('Also check rainfall.'), ' Winter only.')
    await user.click(screen.getByRole('button', { name: 'save' }))
    expect(onEdit).toHaveBeenCalledWith('r1', 'q1', 'Also check rainfall. Winter only.')
  })

  it('saves an edited prompt before the node starts and drops a cancelled edit', async () => {
    const user = userEvent.setup()
    const onEditTask = vi.fn()
    render(<NodePopup node={node} state={{ status: 'queued' }} onClose={() => {}} onEditTask={onEditTask} />)
    await user.click(screen.getByRole('button', { name: 'Edit prompt' }))
    await user.click(screen.getByRole('button', { name: 'cancel' }))
    expect(onEditTask).not.toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: 'Edit prompt' }))
    await user.type(screen.getByDisplayValue('Research Dublin.'), ' In winter.')
    await user.click(screen.getByRole('button', { name: 'save' }))
    expect(onEditTask).toHaveBeenCalledWith('r1', 'Research Dublin. In winter.')
  })
})
