// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen, within } from '@testing-library/react'
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

  it('holds actions while a reply is in flight, with a visible reason', async () => {
    const onAction = vi.fn()
    render(<A2uiSurfaceView content={first} onAction={onAction} busy />)
    await userEvent.setup().click(screen.getByRole('tab', { name: 'Quiz' }))
    const submit = screen.getByRole('button', { name: 'Check my answers' }) as HTMLButtonElement
    expect(submit.disabled).toBe(true)
    expect(screen.getByText('Wait for the current reply to finish')).toBeTruthy()
  })

  it('moves between tabs with the arrow keys and wires tab/panel ids', async () => {
    const user = userEvent.setup()
    render(<A2uiSurfaceView content={first} />)
    const overview = screen.getByRole('tab', { name: 'Overview' })
    overview.focus()
    await user.keyboard('{ArrowLeft}')
    const quiz = screen.getByRole('tab', { name: 'Quiz', selected: true })
    expect(document.activeElement).toBe(quiz)
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe(quiz.id)
    expect(quiz.getAttribute('aria-controls')).toBe(screen.getByRole('tabpanel').id)
  })

  it('renders button labels inline and maps icons onto the quack set', () => {
    const content: SurfaceContent = {
      surface_id: 'misc',
      components: [
        { id: 'root', component: 'Row', children: ['ok', 'nope', 'b'] },
        { id: 'ok', component: 'Icon', name: 'check' },
        { id: 'nope', component: 'Icon', name: 'accountCircle' },
        { id: 'b', component: 'Button', child: 'l', action: { event: { name: 'x' } } },
        { id: 'l', component: 'Text', text: '**Go** [now](https://example.com)' },
      ],
    }
    const { container } = render(<A2uiSurfaceView content={content} />)
    expect(container.querySelectorAll('svg')).toHaveLength(1)
    const button = screen.getByRole('button')
    expect(button.querySelector('p, a')).toBeNull()
    expect(container.textContent).not.toContain('account_circle')
  })

  it('shares picks between two views of one surface (inline card and panel) and never rewinds to an older revision', async () => {
    const user = userEvent.setup()
    render(<><div data-testid="inline"><A2uiSurfaceView content={first} revision={1} persistKey="t-shared" /></div>
      <div data-testid="panel"><A2uiSurfaceView content={graded} revision={2} persistKey="t-shared" /></div></>)
    const panel = within(screen.getByTestId('panel'))
    const inline = within(screen.getByTestId('inline'))
    await user.click(panel.getByRole('tab', { name: 'Quiz' }))
    await user.click(panel.getByLabelText(/That one skill is skipped/))
    await user.click(inline.getByRole('tab', { name: 'Quiz' }))
    expect((inline.getByLabelText(/That one skill is skipped/) as HTMLInputElement).checked).toBe(true)
    expect(inline.getByRole('button', { name: 'Score: 2/4' })).toBeTruthy()
  })
})
