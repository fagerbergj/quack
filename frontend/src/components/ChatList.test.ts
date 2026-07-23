import { describe, it, expect } from 'vitest'
import { filterChats } from '../lib/chatFilters'
import { isGithubChat } from '../lib/github'
import type { ChatSummary } from '../api'

// No @testing-library/react in this repo - ChatList's filter/facet logic
// lives in ../lib/chatFilters and ../lib/github (see their own test files for
// the bulk of the coverage); this file covers the origin-filter wiring
// ChatList's badge/facet row depends on directly.

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

const CHATS: ChatSummary[] = [
  chat({ id: 'direct-1', title: 'Direct chat' }),
  chat({ id: 'github-acme-widget-1', title: 'GitHub chat', github_repo: 'acme/widget', github_url: 'https://github.com/acme/widget/issues/1' }),
]

describe('filterChats (origin facet)', () => {
  it('no selection returns every chat unchanged', () => {
    expect(filterChats(CHATS, { q: '', selected: {} })).toEqual(CHATS)
  })

  it('origin=github narrows to github-origin chats only', () => {
    const result = filterChats(CHATS, { q: '', selected: { origin: ['github'] } })
    expect(result.map(c => c.id)).toEqual(['github-acme-widget-1'])
  })

  it('origin=direct narrows to non-github chats only', () => {
    const result = filterChats(CHATS, { q: '', selected: { origin: ['direct'] } })
    expect(result.map(c => c.id)).toEqual(['direct-1'])
  })

  it('is empty when no chat matches the filter', () => {
    const result = filterChats([CHATS[1]], { q: '', selected: { origin: ['direct'] } })
    expect(result).toEqual([])
  })
})

describe('origin badge signal (isGithubChat)', () => {
  it('is true for the github row and false for the direct row', () => {
    expect(isGithubChat(CHATS[0])).toBe(false)
    expect(isGithubChat(CHATS[1])).toBe(true)
  })
})
