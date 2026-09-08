// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { NodeMemoriesPanel } from './NodeMemoriesPanel'
import { client } from '../generated/client.gen'

client.setConfig({ baseUrl: 'http://localhost' })

afterEach(cleanup)

// jsdom doesn't implement <dialog> (no showModal/close, no `open`
// reflection) - stub both, same as ArtifactPanel.rtl.test.tsx, so
// getByRole('dialog')/its contents aren't treated as hidden.
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) { this.setAttribute('open', '') }
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) { this.removeAttribute('open') }
})

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
}

describe('NodeMemoriesPanel', () => {
  it('lists received memories with source and judge vote', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({
      memories: [{ id: 'm1', source: 'prefill', content: 'retry on 5xx', tier: 'verified', vote: 'supported' }],
    }))
    vi.stubGlobal('fetch', fetchMock)
    render(<NodeMemoriesPanel chatId="c1" nodeId="n1" onClose={() => {}} />)
    await screen.findByText('retry on 5xx')
    expect(screen.getByText('prefill')).toBeTruthy()
    expect(screen.getByText('supported')).toBeTruthy()
    vi.unstubAllGlobals()
  })

  it('refetches when judgeRounds changes (live update on round completion)', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({ memories: [] }))
      .mockResolvedValueOnce(jsonResponse({ memories: [{ id: 'm1', source: 'prefill', content: 'new fact', vote: 'supported' }] }))
    vi.stubGlobal('fetch', fetchMock)
    const { rerender } = render(<NodeMemoriesPanel chatId="c1" nodeId="n1" judgeRounds={1} onClose={() => {}} />)
    await screen.findByText('This node received no memories')
    rerender(<NodeMemoriesPanel chatId="c1" nodeId="n1" judgeRounds={2} onClose={() => {}} />)
    await screen.findByText('new fact')
    expect(fetchMock).toHaveBeenCalledTimes(2)
    vi.unstubAllGlobals()
  })

  it('a manual vote click sends the request and updates optimistically', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({ memories: [{ id: 'm1', source: 'prefill', content: 'a fact' }] }))
      .mockResolvedValueOnce(jsonResponse({ id: 'm1', content: 'a fact', bucket: 'repo:x', author: 'a', timestamp: '2026-01-01T00:00:00Z', kind: 'repo', own_vote: 'up' }))
    vi.stubGlobal('fetch', fetchMock)
    render(<NodeMemoriesPanel chatId="c1" nodeId="n1" onClose={() => {}} />)
    await screen.findByText('a fact')
    await userEvent.click(screen.getByRole('button', { name: 'Upvote' }))
    expect(fetchMock).toHaveBeenCalledTimes(2)
    const voteRequest = fetchMock.mock.calls[1][0] as Request
    expect(voteRequest.method).toBe('POST')
    expect(voteRequest.url).toContain('/api/v1/memories/m1/vote')
    vi.unstubAllGlobals()
  })
})
