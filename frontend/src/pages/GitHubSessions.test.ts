import { describe, it, expect } from 'vitest'
import { groupGithubChats, isGithubChat } from './GitHubSessions'
import type { ChatSummary } from '../api'

// Vitest here runs with no DOM-rendering harness (no @testing-library/react in
// this repo — see Chat.test.ts for the same pure-function convention), so
// coverage targets the page's exported filter/group/sort logic rather than a
// rendered tree.

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

describe('isGithubChat', () => {
  it('is true when github_url is set', () => {
    expect(isGithubChat(chat({ id: 'random', github_url: 'https://github.com/a/b/issues/1' }))).toBe(true)
  })

  it('falls back to the id prefix when github_url is absent', () => {
    expect(isGithubChat(chat({ id: 'github-acme-widget-9' }))).toBe(true)
  })

  it('is false for an ordinary chat', () => {
    expect(isGithubChat(chat({ id: 'a1b2c3' }))).toBe(false)
  })
})

describe('groupGithubChats', () => {
  it('filters out non-github chats', () => {
    const chats = [
      chat({ id: 'plain-1' }),
      chat({
        id: 'github-acme-widget-1',
        github_repo: 'acme/widget',
        github_url: 'https://github.com/acme/widget/issues/1',
      }),
    ]
    const groups = groupGithubChats(chats)
    expect(groups).toHaveLength(1)
    expect(groups[0].chats).toHaveLength(1)
    expect(groups[0].chats[0].id).toBe('github-acme-widget-1')
  })

  it('groups by github_repo and sorts groups + chats by updated_at desc', () => {
    const chats: ChatSummary[] = [
      chat({
        id: 'github-acme-a-1',
        github_repo: 'acme/a',
        github_url: 'https://github.com/acme/a/issues/1',
        updated_at: '2026-07-10T00:00:00Z',
      }),
      chat({
        id: 'github-acme-b-1',
        github_repo: 'acme/b',
        github_url: 'https://github.com/acme/b/issues/1',
        updated_at: '2026-07-12T00:00:00Z', // most recently updated repo
      }),
      chat({
        id: 'github-acme-a-2',
        github_repo: 'acme/a',
        github_url: 'https://github.com/acme/a/pull/2',
        updated_at: '2026-07-11T00:00:00Z', // newer than a-1, within the same repo
      }),
    ]
    const groups = groupGithubChats(chats)
    expect(groups.map(g => g.repo)).toEqual(['acme/b', 'acme/a'])
    expect(groups[1].chats.map(c => c.id)).toEqual(['github-acme-a-2', 'github-acme-a-1'])
  })

  it('returns an empty list when there are no github chats', () => {
    expect(groupGithubChats([chat({ id: 'plain-1' }), chat({ id: 'plain-2' })])).toEqual([])
  })

  it('exposes github_url per chat for the row link href', () => {
    const url = 'https://github.com/acme/widget/pull/42'
    const groups = groupGithubChats([
      chat({ id: 'github-acme-widget-42', github_repo: 'acme/widget', github_url: url }),
    ])
    expect(groups[0].chats[0].github_url).toBe(url)
  })
})
