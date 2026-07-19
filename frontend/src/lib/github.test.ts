import { describe, it, expect } from 'vitest'
import { isGithubChat, parseGithubRef } from './github'
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

describe('parseGithubRef', () => {
  it('parses an issue URL', () => {
    const ref = parseGithubRef(chat({ github_url: 'https://github.com/acme/widget/issues/249', github_repo: 'acme/widget' }))
    expect(ref).toEqual({ repo: 'acme/widget', kind: 'issue', number: 249 })
  })

  it('parses a pull request URL', () => {
    const ref = parseGithubRef(chat({ github_url: 'https://github.com/acme/widget/pull/257', github_repo: 'acme/widget' }))
    expect(ref).toEqual({ repo: 'acme/widget', kind: 'pr', number: 257 })
  })

  it('falls back to the repo parsed from the URL when github_repo is absent', () => {
    const ref = parseGithubRef(chat({ github_url: 'https://github.com/acme/widget/issues/1' }))
    expect(ref?.repo).toBe('acme/widget')
  })

  it('is undefined for a non-github chat', () => {
    expect(parseGithubRef(chat({}))).toBeUndefined()
  })

  it('is undefined for an unrecognized URL shape', () => {
    expect(parseGithubRef(chat({ github_url: 'https://github.com/acme/widget' }))).toBeUndefined()
  })
})
