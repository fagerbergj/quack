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

// Namespaces facets from origin.labels dimensions (e.g. "tags", "folder") so they can't collide with FACET_KEYS;
// the github repo/type facets stay until the GitHub extension stamps origin itself.
const LABEL_FACET_PREFIX = 'label:'

// originLabelDimensions lists every distinct origin.labels key present
// across chats, in a stable (alphabetical) order.
function originLabelDimensions(chats: ChatSummary[]): string[] {
  const dims = new Set<string>()
  for (const c of chats) {
    for (const dim of Object.keys(c.origin?.labels ?? {})) dims.add(dim)
  }
  return Array.from(dims).sort()
}

// Tallies one dimension's values across chats, keyed by LabelValue.value, carrying display text (falling back
// to value, as the API does).
function countByLabelValue(chats: ChatSummary[], dim: string): Map<string, { count: number; display: string }> {
  const counts = new Map<string, { count: number; display: string }>()
  for (const c of chats) {
    for (const lv of c.origin?.labels?.[dim] ?? []) {
      const existing = counts.get(lv.value)
      counts.set(lv.value, { count: (existing?.count ?? 0) + 1, display: lv.display ?? lv.value })
    }
  }
  return counts
}

const STATUS_LABELS: Record<ChatSummary['status'], string> = {
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

// Facet groups with per-option counts from the chats present; a facet with no options is omitted.
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

  // One facet per origin.labels dimension actually present, generic to whichever extension supplied it.
  for (const dim of originLabelDimensions(chats)) {
    const counts = countByLabelValue(chats, dim)
    if (counts.size === 0) continue
    facets.push({
      key: LABEL_FACET_PREFIX + dim,
      label: dim.charAt(0).toUpperCase() + dim.slice(1),
      options: Array.from(counts, ([value, { count, display }]) => ({ value, label: display, count })).sort((a, b) =>
        a.label.localeCompare(b.label),
      ),
    })
  }

  return facets
}

// AND across facets with an active selection, OR within one; an empty selection imposes nothing.
// Iterates every key in `selected`, not just FACET_KEYS, so dynamic label:<dimension> selections apply too.
export function matchesFacets(chat: ChatSummary, selected: SelectedFacets): boolean {
  for (const key of Object.keys(selected)) {
    const values = selected[key]
    if (!values || values.length === 0) continue
    if (key.startsWith(LABEL_FACET_PREFIX)) {
      const dim = key.slice(LABEL_FACET_PREFIX.length)
      const chatValues = (chat.origin?.labels?.[dim] ?? []).map(lv => lv.value)
      if (!chatValues.some(v => values.includes(v))) return false
      continue
    }
    if (!(FACET_KEYS as readonly string[]).includes(key)) continue // unknown key: no constraint
    const value = facetValue(chat, key as FacetKey)
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

// Round-trip the search box and facet selection through the URL query, so a filtered view is shareable.
// label:<dimension> keys are data-driven, so they're read and written generically, not via the FACET_KEYS allowlist.
export function parseFilterState(search: string): FilterState {
  const params = new URLSearchParams(search)
  const q = params.get('q') ?? ''
  const selected: SelectedFacets = {}
  for (const key of FACET_KEYS) {
    const raw = params.get(key)
    if (raw) selected[key] = raw.split(',').filter(Boolean)
  }
  for (const [key, raw] of params) {
    if (key === 'q' || !key.startsWith(LABEL_FACET_PREFIX) || !raw) continue
    selected[key] = raw.split(',').filter(Boolean)
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
  for (const [key, values] of Object.entries(state.selected)) {
    if (!key.startsWith(LABEL_FACET_PREFIX) || !values || values.length === 0) continue
    params.set(key, values.join(','))
  }
  return params.toString()
}
