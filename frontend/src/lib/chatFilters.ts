import type { ChatSummary } from '../api'
import type { Facet } from '../components/FilterPanel'
import { isGithubChat, parseGithubRef } from './github'

export type SelectedFacets = Record<string, string[]>

export interface FilterState {
  q: string
  selected: SelectedFacets
}

// The facet keys understood by computeFacets/matchesFacets/URL (de)serialization.
// Order here is display order and URL param order.
const FACET_KEYS = ['origin', 'status', 'repo', 'type'] as const
type FacetKey = (typeof FACET_KEYS)[number]

const STATUS_LABELS: Record<ChatSummary['status'], string> = {
  queued: 'Queued',
  running: 'Running',
  needs_input: 'Needs input',
  failed: 'Failed',
  idle: 'Idle',
}

function facetValue(chat: ChatSummary, key: FacetKey): string | undefined {
  switch (key) {
    case 'origin':
      return isGithubChat(chat) ? 'github' : 'direct'
    case 'status':
      return chat.status
    case 'repo':
      return parseGithubRef(chat)?.repo
    case 'type':
      return parseGithubRef(chat)?.kind
  }
}

function countBy(chats: ChatSummary[], key: FacetKey): Map<string, number> {
  const counts = new Map<string, number>()
  for (const c of chats) {
    const v = facetValue(c, key)
    if (v !== undefined) counts.set(v, (counts.get(v) ?? 0) + 1)
  }
  return counts
}

// computeFacets derives the facet groups (with per-option counts) from the
// chats actually present. A facet with no options (e.g. no GitHub-originated
// chats in the list) is omitted.
export function computeFacets(chats: ChatSummary[]): Facet[] {
  const facets: Facet[] = []

  const originCounts = countBy(chats, 'origin')
  facets.push({
    key: 'origin',
    label: 'Origin',
    options: [
      { value: 'direct', label: 'Direct', count: originCounts.get('direct') ?? 0 },
      { value: 'github', label: 'GitHub', count: originCounts.get('github') ?? 0 },
    ],
  })

  const statusCounts = countBy(chats, 'status')
  if (statusCounts.size > 0) {
    facets.push({
      key: 'status',
      label: 'Status',
      options: Array.from(statusCounts, ([value, count]) => ({
        value,
        label: STATUS_LABELS[value as ChatSummary['status']] ?? value,
        count,
      })).sort((a, b) => a.label.localeCompare(b.label)),
    })
  }

  const repoCounts = countBy(chats, 'repo')
  if (repoCounts.size > 0) {
    facets.push({
      key: 'repo',
      label: 'Repo',
      options: Array.from(repoCounts, ([value, count]) => ({ value, label: value, count })).sort((a, b) =>
        a.label.localeCompare(b.label),
      ),
    })
  }

  const typeCounts = countBy(chats, 'type')
  if (typeCounts.size > 0) {
    facets.push({
      key: 'type',
      label: 'Type',
      options: [
        { value: 'issue', label: 'Issue', count: typeCounts.get('issue') ?? 0 },
        { value: 'pr', label: 'PR', count: typeCounts.get('pr') ?? 0 },
      ].filter(o => o.count > 0),
    })
  }

  return facets
}

// matchesFacets: a chat matches when it satisfies every facet that has an
// active selection (AND across facets); within a facet, any selected value
// matches (OR). An empty/absent selection for a facet imposes no constraint.
export function matchesFacets(chat: ChatSummary, selected: SelectedFacets): boolean {
  for (const key of FACET_KEYS) {
    const values = selected[key]
    if (!values || values.length === 0) continue
    const value = facetValue(chat, key)
    if (value === undefined || !values.includes(value)) return false
  }
  return true
}

function matchesSearch(chat: ChatSummary, q: string): boolean {
  if (!q) return true
  return (chat.title ?? '').toLowerCase().includes(q.toLowerCase())
}

// filterChats applies the search query and every active facet together.
export function filterChats(chats: ChatSummary[], state: FilterState): ChatSummary[] {
  const q = state.q.trim()
  return chats.filter(c => matchesSearch(c, q) && matchesFacets(c, state.selected))
}

// parseFilterState / serializeFilterState round-trip the search box + facet
// selection through the URL query string (?q=…&origin=github&status=running&
// repo=owner%2Frepo&type=pr) so a filtered view is shareable and bookmarkable.
export function parseFilterState(search: string): FilterState {
  const params = new URLSearchParams(search)
  const q = params.get('q') ?? ''
  const selected: SelectedFacets = {}
  for (const key of FACET_KEYS) {
    const raw = params.get(key)
    if (raw) selected[key] = raw.split(',').filter(Boolean)
  }
  return { q, selected }
}

export function serializeFilterState(state: FilterState): string {
  const params = new URLSearchParams()
  if (state.q.trim()) params.set('q', state.q)
  for (const key of FACET_KEYS) {
    const values = state.selected[key]
    if (values && values.length > 0) params.set(key, values.join(','))
  }
  return params.toString()
}
