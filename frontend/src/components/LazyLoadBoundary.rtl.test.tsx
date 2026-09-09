// @vitest-environment jsdom
import { Suspense, lazy } from 'react'
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { LazyLoadBoundary } from './LazyLoadBoundary'

afterEach(cleanup)

// A stale deploy or an offline tab makes a route chunk's dynamic import
// reject exactly like this - the case Suspense alone can't recover from.
const BrokenChunk = lazy(() => Promise.reject(new Error('Failed to fetch dynamically imported module')))

describe('LazyLoadBoundary', () => {
  it('shows a reload fallback instead of white-screening when the lazy import rejects', async () => {
// React logs the caught error to console.error - expected here, not a test failure.
    vi.spyOn(console, 'error').mockImplementation(() => {})
    render(
      <LazyLoadBoundary>
        <Suspense fallback={null}>
          <BrokenChunk />
        </Suspense>
      </LazyLoadBoundary>
    )
    expect(await screen.findByRole('button', { name: 'Reload' })).toBeTruthy()
    expect(screen.getByText(/couldn.t load/i)).toBeTruthy()
  })

  it('reloads the page on click', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    const reload = vi.fn()
    vi.stubGlobal('location', { ...window.location, reload })
    const user = userEvent.setup()
    render(
      <LazyLoadBoundary>
        <Suspense fallback={null}>
          <BrokenChunk />
        </Suspense>
      </LazyLoadBoundary>
    )
    await user.click(await screen.findByRole('button', { name: 'Reload' }))
    expect(reload).toHaveBeenCalledOnce()
    vi.unstubAllGlobals()
  })

  it('renders children normally when nothing fails', () => {
    render(<LazyLoadBoundary><div>fine</div></LazyLoadBoundary>)
    expect(screen.getByText('fine')).toBeTruthy()
  })
})
