// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import A2uiSurfaceView from './A2uiSurface'
import { pr1085 } from './A2uiSurface.fixtures'
import type { SurfaceContent } from '../lib/a2ui'

afterEach(cleanup)

const first = pr1085.first
// The stored data_model is unchanged by grading; only the components move.
const graded = { ...pr1085.graded, data_model: first.data_model }

async function pickAnswers(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('tab', { name: 'Quiz' }))
  await user.click(screen.getByLabelText(/That one skill is skipped/))
}

describe('A2uiSurfaceView', () => {
  it('renders the first revision and posts a resolved submit action', async () => {
    const user = userEvent.setup()
    const onAction = vi.fn()
    render(<A2uiSurfaceView content={first} onAction={onAction} />)
    expect(screen.getByRole('heading', { name: /PR #1085/ })).toBeTruthy()
    await pickAnswers(user)
    await user.click(screen.getByRole('button', { name: 'Check my answers' }))
    expect(onAction).toHaveBeenCalledWith({
      surface_id: 'pr-1085-tutor',
      name: 'submit_quiz',
      source_component_id: 'submit',
      context: { answers: { q1: ['b'], q2: [], q3: [], q4: [] } },
    })
  })

  it('applies a graded revision in place, keeping the picks and the open tab', async () => {
    const user = userEvent.setup()
    const { rerender } = render(<A2uiSurfaceView content={first} persistKey="t-inplace" />)
    await pickAnswers(user)
    rerender(<A2uiSurfaceView content={graded} persistKey="t-inplace" />)
    expect(screen.getByRole('button', { name: 'Score: 2/4' })).toBeTruthy()
    expect(screen.getAllByText(/Correct\./).length).toBe(2)
    expect((screen.getByLabelText(/That one skill is skipped/) as HTMLInputElement).checked).toBe(true)
  })

  it('keeps state across a remount with the same persistKey', async () => {
    const user = userEvent.setup()
    const { unmount } = render(<A2uiSurfaceView content={first} persistKey="t-remount" />)
    await pickAnswers(user)
    unmount()
    render(<A2uiSurfaceView content={graded} persistKey="t-remount" />)
    expect(screen.getByRole('tab', { name: 'Quiz', selected: true })).toBeTruthy()
    expect((screen.getByLabelText(/That one skill is skipped/) as HTMLInputElement).checked).toBe(true)
  })

  it('shows the validation error instead of throwing on a bad surface', () => {
    const bad: SurfaceContent = { surface_id: 'bad', components: [{ id: 'root', component: 'Text', text: 1 }] }
    render(<A2uiSurfaceView content={bad} />)
    expect(screen.getByRole('alert').textContent).toMatch(/Could not render this surface/)
  })
})
