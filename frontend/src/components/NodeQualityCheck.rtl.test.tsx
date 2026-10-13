// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { NodeQualityCheck } from './NodeQualityCheck'
import { client } from '../generated/client.gen'

const START = Date.parse('2026-10-12T18:30:00Z')
const saved = (min: number) => new Date(START + min * 60_000).toISOString()
const summary = (name: string, round: number, min: number) => ({
  name, kind: 'judge_round', class: 'structured', latest_revision: 1, revisions: [],
  lineage: { node_id: 'synth-1', round, author: 'judge', saved_at: saved(min) },
})
const bodies: Record<string, unknown> = {
  'judge_round:e-1-synth-1-1': { round: 1, passed: false, score: 0.57, criteria: [{ name: 'grounded', score: 1 }, { name: 'specifics_cited', score: 0.57, feedback: '4 of 7 specifics cited' }] },
  'judge_round:e-1-synth-1-2': { round: 2, passed: true, score: 0.96, criteria: [{ name: 'specifics_cited', score: 0.96 }] },
}

function stub(bodyStatus = 200) {
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = decodeURIComponent(input instanceof Request ? input.url : String(input))
    if (url.endsWith('/artifacts')) {
      return new Response(JSON.stringify({ data: [summary('judge_round:e-1-synth-1-2', 2, 3), summary('judge_round:e-1-synth-1-1', 1, 1), summary('judge_round:e-0-synth-1-1', 1, -90)] }), { headers: { 'Content-Type': 'application/json' } })
    }
    const name = Object.keys(bodies).find(n => url.includes(`/artifacts/${n}`))
    return name && bodyStatus === 200 ? new Response(JSON.stringify(bodies[name])) : new Response('{}', { status: 500 })
  })
  vi.stubGlobal('fetch', fetch)
  return fetch
}

beforeEach(() => client.setConfig({ baseUrl: 'http://localhost' }))
afterEach(() => { cleanup(); vi.unstubAllGlobals() })

const props = { chatId: 'chat-qc', nodeId: 'synth-1', startedAt: START, finishedAt: START + 5 * 60_000 }

describe('NodeQualityCheck', () => {
  it("counts this run's rounds and loads their criteria only when opened", async () => {
    const fetch = stub()
    const user = userEvent.setup()
    render(<NodeQualityCheck {...props} />)

    expect(await screen.findByText('2 rounds')).toBeTruthy()
    expect(fetch).toHaveBeenCalledTimes(1)

    await user.click(screen.getByText('Quality check'))
    expect(await screen.findByText('Round 1')).toBeTruthy()
    expect(screen.getByRole('region', { name: 'Judge round 1' }).textContent).toContain('failed 57%on specifics_cited')
    expect(screen.getByRole('region', { name: 'Judge round 2' }).textContent).toContain('passed 96%')
    expect(screen.getByText('4 of 7 specifics cited')).toBeTruthy()
  })

  it('says so when a round body fails to load', async () => {
    stub(500)
    const user = userEvent.setup()
    render(<NodeQualityCheck {...props} />)
    await user.click(await screen.findByText('Quality check'))
    expect(await screen.findByText("Couldn't load the judge rounds.")).toBeTruthy()
  })

  it('renders nothing for a node without a finished run', () => {
    stub()
    const { container } = render(<NodeQualityCheck {...props} finishedAt={undefined} />)
    expect(container.innerHTML).toBe('')
  })
})
