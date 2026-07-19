import { describe, it, expect } from 'vitest'
import { filterChatsByOrigin } from './ChatList'
import { isGithubChat } from '../pages/GitHubSessions'
import type { ChatSummary } from '../api'

// No @testing-library/react in this repo (see GitHubSessions.test.ts) — coverage
// targets the exported filter logic and the shared isGithubChat signal the
// row badge and filter control both read from.

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

describe('filterChatsByOrigin', () => {
  it('"all" returns every chat unchanged', () => {
    expect(filterChatsByOrigin(CHATS, 'all')).toEqual(CHATS)
  })

  it('"github" narrows to github-origin chats only', () => {
    const result = filterChatsByOrigin(CHATS, 'github')
    expect(result.map(c => c.id)).toEqual(['github-acme-widget-1'])
  })

  it('"direct" narrows to non-github chats only', () => {
    const result = filterChatsByOrigin(CHATS, 'direct')
    expect(result.map(c => c.id)).toEqual(['direct-1'])
  })

  it('is empty when no chat matches the filter', () => {
    expect(filterChatsByOrigin([CHATS[0]], 'github')).toEqual([])
  })
})

describe('origin badge signal (isGithubChat, shared with GitHubSessions)', () => {
  it('is true for the github row and false for the direct row', () => {
    expect(isGithubChat(CHATS[0])).toBe(false)
    expect(isGithubChat(CHATS[1])).toBe(true)
  })
})
