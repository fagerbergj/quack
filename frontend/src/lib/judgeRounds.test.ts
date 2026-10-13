import { describe, it, expect, vi, afterEach } from 'vitest'
import type { ArtifactSummary } from '../api'
import { client } from '../generated/client.gen'
import { failedCriterionLine, nodeJudgeRounds, parseJudgeRound } from './judgeRounds'

// Node's Request rejects the generated client's relative URLs.
client.setConfig({ baseUrl: 'http://localhost' })

const T0 = Date.parse('2026-10-11T15:50:00Z')
const jr = (exec: string, node: string, round: number, min: number): ArtifactSummary => ({
  name: `judge_round:${exec}-${node}-${round}`, kind: 'judge_round', class: 'structured', latest_revision: 1, revisions: [],
  // Nanosecond saved_at, as the server writes it.
  lineage: { node_id: node, round, author: 'judge', saved_at: new Date(T0 + min * 60_000).toISOString().replace('Z', '123456Z') },
})

describe('nodeJudgeRounds', () => {
  // A recurring extension job reuses node id "trends" in every turn; only this run's rounds belong to its card.
  const list = [
    jr('e-aaa', 'trends', 1, -100), jr('e-aaa', 'trends', 2, -98),
    jr('e-bbb', 'trends', 2, 6), jr('e-bbb', 'trends', 1, 4),
    jr('e-bbb', 'other', 1, 5),
  ]

  it('keeps the rounds saved inside the run window, in round order', () => {
    expect(nodeJudgeRounds(list, 'trends', T0, T0 + 10 * 60_000)).toEqual([
      { name: 'judge_round:e-bbb-trends-1', round: 1 },
      { name: 'judge_round:e-bbb-trends-2', round: 2 },
    ])
  })

  it('keeps only the latest execution when a retry lands two in one window', () => {
    const retried = [...list, jr('e-ccc', 'trends', 1, 8)]
    expect(nodeJudgeRounds(retried, 'trends', T0, T0 + 10 * 60_000).map(r => r.name)).toEqual(['judge_round:e-ccc-trends-1'])
  })

  it('finds nothing outside the window', () => {
    expect(nodeJudgeRounds(list, 'trends', T0 + 60 * 60_000, T0 + 70 * 60_000)).toEqual([])
  })
})

describe('parseJudgeRound', () => {
  it('marks the weakest criterion of a failed round as the one that sank it', () => {
    const d = parseJudgeRound({
      round: 1, passed: false, score: 0.57,
      criteria: [{ name: 'grounded', score: 1 }, { name: 'specifics_cited', score: 0.57, feedback: '4 of 7 specifics cited' }, { name: 'clean_output', score: 0.9 }],
    }, 9)!
    expect(d.round).toBe(1)
    expect(d.criteria.filter(c => c.failed).map(c => c.name)).toEqual(['specifics_cited'])
    expect(d.criteria[1].feedback).toBe('4 of 7 specifics cited')
  })

  it('marks nothing in a passed round and falls back to the ref round', () => {
    const d = parseJudgeRound({ passed: true, criteria: [{ name: 'a', score: 0.8 }, { name: 'b' }] }, 2)!
    expect(d.round).toBe(2)
    expect(d.criteria.some(c => c.failed)).toBe(false)
    expect(d.criteria[1].score).toBeUndefined()
  })

  it('rejects a body that is not an object', () => {
    expect(parseJudgeRound('not json', 1)).toBeNull()
    expect(parseJudgeRound([1], 1)).toBeNull()
    expect(parseJudgeRound(null, 1)).toBeNull()
  })
})

describe('failedCriterionLine', () => {
  afterEach(() => { vi.unstubAllGlobals() })
  const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })

  it("names the criterion that sank the node's last round, reading only that round", async () => {
    const list = [jr('e-bbb', 'synth', 1, 1), jr('e-bbb', 'synth', 3, 3), jr('e-bbb', 'synth', 2, 2)]
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = input instanceof Request ? input.url : String(input)
      return Promise.resolve(url.endsWith('/artifacts') ? json({ data: list })
        : json({ passed: false, criteria: [{ name: 'accuracy', score: 0.9 }, { name: 'cites_sources', score: 0.28, feedback: 'Two claims cite nothing.' }] }))
    })
    vi.stubGlobal('fetch', fetchMock)
    expect(await failedCriterionLine('c1', 'synth', T0, T0 + 10 * 60_000)).toBe('Weakest criterion: cites_sources at 28%. Two claims cite nothing.')
    const read = fetchMock.mock.calls.map(([i]) => (i instanceof Request ? i.url : String(i))).find(u => !u.endsWith('/artifacts'))
    expect(decodeURIComponent(read ?? '')).toContain('synth-3')
  })

  it('says so when the node saved no round, or the fetch fails', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json({ data: [] }))))
    expect(await failedCriterionLine('c2', 'synth', T0, T0 + 1)).toBe('No criterion recorded.')
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('offline'))))
    expect(await failedCriterionLine('c3', 'synth', T0, T0 + 1)).toBe("Couldn't load the judge's verdict.")
  })
})
