import { describe, it, expect } from 'vitest'
import {
  computeFacets,
  filterChats,
  matchesFacets,
  parseFilterState,
  serializeFilterState,
  type FilterState,
} from './chatFilters'
import type { ChatSummary } from '../api'

function chat(overrides: Partial<ChatSummary>): ChatSummary {
  return {
    id: 'c1',
    system_prompt: '',
    created_at: '2026-07-10T00:00:00Z',
    updated_at: '2026-07-10T00:00:00Z',
    status: 'idle',
    ...overrides,
  }
}

const DIRECT = chat({ id: 'direct-1', title: 'Direct chat', status: 'idle' })
const GH_ISSUE = chat({
  id: 'gh-issue-1',
  title: 'Issue chat',
  status: 'running',
  github_repo: 'acme/widget',
  github_url: 'https://github.com/acme/widget/issues/249',
})
const GH_PR = chat({
  id: 'gh-pr-1',
  title: 'PR chat',
  status: 'failed',
  github_repo: 'acme/other',
  github_url: 'https://github.com/acme/other/pull/257',
})
const CHATS: ChatSummary[] = [DIRECT, GH_ISSUE, GH_PR]

describe('computeFacets', () => {
  it('includes origin, status, repo, type facets with counts', () => {
    const facets = computeFacets(CHATS)
    const byKey = Object.fromEntries(facets.map(f => [f.key, f]))
    expect(byKey.origin.options).toEqual([
      { value: 'direct', label: 'Direct', count: 1 },
      { value: 'github', label: 'GitHub', count: 2 },
    ])
    expect(byKey.status.options.map(o => o.value).sort()).toEqual(['failed', 'idle', 'running'])
    expect(byKey.repo.options.map(o => o.value).sort()).toEqual(['acme/other', 'acme/widget'])
    expect(byKey.type.options).toEqual([
      { value: 'issue', label: 'Issue', count: 1 },
      { value: 'pr', label: 'PR', count: 1 },
    ])
  })

  it('omits a facet with no options (e.g. no github chats present)', () => {
    const facets = computeFacets([DIRECT])
    const keys = facets.map(f => f.key)
    expect(keys).not.toContain('repo')
    expect(keys).not.toContain('type')
    expect(keys).toContain('origin')
  })

  // #417: queued (admitted but waiting behind max_active_runs) is a distinct
  // ChatStatus from running — it must appear in the status facet with its own
  // label, not fold into or get dropped alongside running.
  it('labels a queued chat distinctly from a running one', () => {
    const facets = computeFacets([DIRECT, chat({ id: 'q1', status: 'queued' })])
    const byKey = Object.fromEntries(facets.map(f => [f.key, f]))
    const byValue = Object.fromEntries(byKey.status.options.map((o: { value: string; label: string }) => [o.value, o.label]))
    expect(byValue.queued).toBe('Queued')
  })
})

describe('matchesFacets', () => {
  it('has no constraint when selected is empty', () => {
    expect(matchesFacets(DIRECT, {})).toBe(true)
  })

  it('ANDs across facets: must satisfy every facet with an active selection', () => {
    expect(matchesFacets(GH_ISSUE, { origin: ['github'], type: ['issue'] })).toBe(true)
    expect(matchesFacets(GH_ISSUE, { origin: ['github'], type: ['pr'] })).toBe(false)
  })

  it('ORs within a facet: any selected value matches', () => {
    expect(matchesFacets(GH_ISSUE, { status: ['idle', 'running'] })).toBe(true)
    expect(matchesFacets(GH_ISSUE, { status: ['idle', 'failed'] })).toBe(false)
  })

  it('a chat without a repo/type value never matches a repo/type selection', () => {
    expect(matchesFacets(DIRECT, { repo: ['acme/widget'] })).toBe(false)
    expect(matchesFacets(DIRECT, { type: ['issue'] })).toBe(false)
  })
})

describe('filterChats', () => {
  it('combines search + facets', () => {
    const state: FilterState = { q: 'issue', selected: { origin: ['github'] } }
    expect(filterChats(CHATS, state).map(c => c.id)).toEqual(['gh-issue-1'])
  })

  it('search is case-insensitive and matches title substrings', () => {
    const state: FilterState = { q: 'CHAT', selected: {} }
    expect(filterChats(CHATS, state).map(c => c.id).sort()).toEqual(['direct-1', 'gh-issue-1', 'gh-pr-1'])
  })

  it('empty state returns every chat', () => {
    expect(filterChats(CHATS, { q: '', selected: {} })).toEqual(CHATS)
  })
})

describe('parseFilterState / serializeFilterState round trip', () => {
  const cases: FilterState[] = [
    { q: '', selected: {} },
    { q: 'dublin', selected: {} },
    { q: '', selected: { origin: ['github'] } },
    { q: '', selected: { status: ['running', 'failed'] } },
    { q: 'widget', selected: { origin: ['github'], repo: ['acme/widget'], type: ['issue'] } },
  ]

  it.each(cases)('round-trips %j', state => {
    const qs = serializeFilterState(state)
    expect(parseFilterState(qs)).toEqual(state)
  })

  it('serializes to the documented query param shape', () => {
    const qs = serializeFilterState({
      q: 'foo',
      selected: { origin: ['github'], status: ['running'], repo: ['owner/repo'], type: ['pr'] },
    })
    const params = new URLSearchParams(qs)
    expect(params.get('q')).toBe('foo')
    expect(params.get('origin')).toBe('github')
    expect(params.get('status')).toBe('running')
    expect(params.get('repo')).toBe('owner/repo')
    expect(params.get('type')).toBe('pr')
  })

  it('parses a leading "?" the same as a bare query string', () => {
    expect(parseFilterState('?q=x&origin=direct')).toEqual({ q: 'x', selected: { origin: ['direct'] } })
  })

  it('ignores unrecognized params', () => {
    expect(parseFilterState('?bogus=1')).toEqual({ q: '', selected: {} })
  })
})
