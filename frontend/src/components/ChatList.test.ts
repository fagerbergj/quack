// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'

import { filterChats } from '../lib/chatFilters'
import { isGithubChat } from '../lib/github'
import { ChatList, githubStateBadgeClass, githubStateLabel } from './ChatList'
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

// #759 item 4: the repo badge drops the owner (identical on every row here)
// and keeps just the name - the owner is still available in the title
// attribute for the day a chat's repo isn't fagerbergj's.
describe('repo badge text', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  it('shows only the repo name, with the full owner/name in the title', () => {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(ChatList, {
        chats: CHATS,
        activeChatId: null,
        open: true,
        onSelect: () => {},
        onNewChat: () => {},
        onDelete: () => {},
        onCloseMobile: () => {},
      }))
    })
    const badge = host!.querySelector('a[title="acme/widget"]')
    expect(badge?.textContent).toBe('widget')
  })
})

// #736: the sidebar is server-paginated - "Load more" only appears once the
// parent signals a next page exists (hasMoreChats), and clicking it defers
// to the parent's fetch (onLoadMoreChats), not a local re-fetch.
describe('ChatList "Load more" affordance', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  function renderList(props: Partial<Parameters<typeof ChatList>[0]> = {}) {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(ChatList, {
        chats: CHATS,
        activeChatId: null,
        open: true,
        onSelect: () => {},
        onNewChat: () => {},
        onDelete: () => {},
        onCloseMobile: () => {},
        ...props,
      }))
    })
  }

  it('is absent when hasMoreChats is not set', () => {
    renderList()
    expect(host!.textContent).not.toContain('Load more')
  })

  it('appears and calls onLoadMoreChats on click when hasMoreChats is true', () => {
    const onLoadMoreChats = vi.fn()
    renderList({ hasMoreChats: true, onLoadMoreChats })

    const button = Array.from(host!.querySelectorAll('button')).find(b => b.textContent === 'Load more')
    expect(button).toBeTruthy()
    act(() => { button!.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
    expect(onLoadMoreChats).toHaveBeenCalledTimes(1)
  })

  it('shows a disabled loading state while loadingMoreChats is true', () => {
    renderList({ hasMoreChats: true, loadingMoreChats: true })
    const button = Array.from(host!.querySelectorAll('button')).find(b => b.textContent === 'Loading…')
    expect(button).toBeTruthy()
    expect(button!.disabled).toBe(true)
  })
})

describe('github_state badge', () => {
  it.each([
    { state: 'open', label: '◉ open', cls: 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400' },
    { state: 'closed', label: '✕ closed', cls: 'bg-red-100 text-red-600 dark:bg-red-900/30 dark:text-red-400' },
    { state: 'merged', label: '✓ merged', cls: 'bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-400' },
    { state: 'draft', label: '⊘ draft', cls: 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-500' },
  ])('renders correct class and label for state="$state"', ({ state, label, cls }) => {
    expect(githubStateBadgeClass(state)).toBe(cls)
    expect(githubStateLabel(state)).toBe(label)
  })

  it.each([
    { input: 'open', expected: 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400' },
    { input: 'closed', expected: 'bg-red-100 text-red-600 dark:bg-red-900/30 dark:text-red-400' },
    { input: 'merged', expected: 'bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-400' },
    { input: 'draft', expected: 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-500' },
    { input: '', expected: '' },
  ])('githubStateBadgeClass("$input") returns classes or empty', ({ input, expected }) => {
    expect(githubStateBadgeClass(input)).toBe(expected)
  })

  it.each([
    { input: 'open', expected: '◉ open' },
    { input: 'closed', expected: '✕ closed' },
    { input: 'merged', expected: '✓ merged' },
    { input: 'draft', expected: '⊘ draft' },
    { input: '', expected: '' },
    { input: 'unknown', expected: '' },
  ])('githubStateLabel("$input") returns label or empty string', ({ input, expected }) => {
    expect(githubStateLabel(input)).toBe(expected)
  })

  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  function renderGithubChats(githubState: ChatSummary['github_state']) {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    const githubChats: ChatSummary[] = [chat({ id: 'pr-1', title: 'PR chat', github_repo: 'acme/widget', github_url: 'https://github.com/acme/widget/pull/42', github_state: githubState })]
    act(() => {
      root!.render(createElement(ChatList, { chats: githubChats, activeChatId: null, open: true, onSelect: () => {}, onNewChat: () => {}, onDelete: () => {}, onCloseMobile: () => {} }))
    })
  }

  it.each([
    { state: 'open', expected: ['◉ open'] },
    { state: 'closed', expected: ['✕ closed'] },
    { state: 'merged', expected: ['✓ merged'] },
    { state: 'draft', expected: ['⊘ draft'] },
  ])('renders state badge text when github_state="$state"', ({ state, expected }) => {
    renderGithubChats(state as ChatSummary['github_state'])
    const allText = host!.textContent ?? ''
    for (const t of expected) {
      expect(allText).toContain(t)
    }
  })

  it('does not render a badge when github_state is undefined', () => {
    renderGithubChats(undefined)
    expect(host!.textContent).not.toContain('◉ open')
    expect(host!.textContent).not.toContain('✕ closed')
    expect(host!.textContent).not.toContain('✓ merged')
    expect(host!.textContent).not.toContain('⊘ draft')
  })

  it('does not render a badge when github_state is empty string', () => {
    renderGithubChats('' as unknown as ChatSummary['github_state'])
    expect(host!.textContent).not.toContain('◉ open')
    expect(host!.textContent).not.toContain('✕ closed')
  })

  it('renders state badge alongside Issue/PR badge', () => {
    renderGithubChats('merged')
    const spans = host!.querySelectorAll('span[class*="rounded"]')
    const badgeTexts: string[] = []
    for (const s of spans) {
      const t = (s as HTMLElement).textContent?.trim() ?? ''
      if (t && ['◉ open', '✕ closed', '✓ merged', '⊘ draft'].includes(t)) {
        badgeTexts.push(t)
      }
    }
    expect(badgeTexts.length).toBe(1)
    expect(badgeTexts[0]).toBe('✓ merged')
  })
})

// The × is a two-stage trash: archive on an active row, hard-delete on an
// archived one. This is the regression guard - a mis-wiring here would
// silently hard-delete an active chat on a single click.
describe('ChatRow trash button (archive vs. hard delete)', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
    vi.restoreAllMocks()
  })

  // #809: the active list never carries archived rows, so an archived fixture
  // is passed via archivedChats (the section's own server-scoped list), not chats.
  function renderList(chats: ChatSummary[], props: Partial<Parameters<typeof ChatList>[0]> = {}) {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(ChatList, {
        chats,
        activeChatId: null,
        open: true,
        onSelect: () => {},
        onNewChat: () => {},
        onDelete: () => {},
        onCloseMobile: () => {},
        ...props,
      }))
    })
  }

  function expandArchived() {
    const btn = Array.from(host!.querySelectorAll('button')).find(b => b.textContent?.includes('Archived'))
    act(() => { btn!.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
  }

  function click(el: Element | null | undefined) {
    act(() => { el!.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
  }

  it('on an active row, archives and does NOT delete', () => {
    const onArchive = vi.fn()
    const onDelete = vi.fn()
    renderList([chat({ id: 'a1', title: 'Active chat' })], { onArchive, onDelete })

    const trash = host!.querySelector('button[aria-label="Archive chat"]')
    expect(trash).toBeTruthy()
    click(trash)

    expect(onArchive).toHaveBeenCalledWith('a1')
    expect(onDelete).not.toHaveBeenCalled()
  })

  it('on an archived row, permanently deletes after confirmation', () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const onDelete = vi.fn()
    renderList([], { onDelete, archivedChats: [chat({ id: 'a2', title: 'Archived chat', archived: true })] })
    expandArchived()

    const trash = host!.querySelector('button[aria-label="Delete chat permanently"]')
    expect(trash).toBeTruthy()
    click(trash)

    expect(window.confirm).toHaveBeenCalledOnce()
    expect(onDelete).toHaveBeenCalledWith('a2', expect.anything())
  })

  it('on an archived row, does nothing if the confirmation is dismissed', () => {
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    const onDelete = vi.fn()
    renderList([], { onDelete, archivedChats: [chat({ id: 'a3', archived: true })] })
    expandArchived()

    click(host!.querySelector('button[aria-label="Delete chat permanently"]'))

    expect(onDelete).not.toHaveBeenCalled()
  })

  it('the aria-label/title differ between an active row and an archived row', () => {
    renderList([chat({ id: 'a4', title: 'Active' })], {
      archivedChats: [chat({ id: 'a5', title: 'Archived', archived: true })],
    })
    expandArchived()

    const archiveBtn = host!.querySelector('button[aria-label="Archive chat"]')
    const deleteBtn = host!.querySelector('button[aria-label="Delete chat permanently"]')
    expect(archiveBtn).toBeTruthy()
    expect(deleteBtn).toBeTruthy()
    expect(archiveBtn!.getAttribute('title')).toBe('Archive chat')
    expect(deleteBtn!.getAttribute('title')).toBe('Delete chat permanently')
  })

  it('an archived row shows a Restore control that unarchives', () => {
    const onUnarchive = vi.fn()
    renderList([], { onUnarchive, archivedChats: [chat({ id: 'a6', archived: true })] })
    expandArchived()

    const restore = host!.querySelector('button[aria-label="Unarchive chat"]')
    expect(restore).toBeTruthy()
    click(restore)

    expect(onUnarchive).toHaveBeenCalledWith('a6')
  })

  it('an active row has no Restore control', () => {
    renderList([chat({ id: 'a7' })])
    expect(host!.querySelector('button[aria-label="Unarchive chat"]')).toBeNull()
  })
})

// #809 test case 5: the sidebar's initial load never fetches archived chats -
// archivedChats starts undefined/unfetched, and only expanding the Archived
// section triggers the parent's own fetch for it.
describe('Archived section fetches its own list only on expand', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  function renderList(props: Partial<Parameters<typeof ChatList>[0]> = {}) {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(ChatList, {
        chats: [chat({ id: 'c1' })],
        activeChatId: null,
        open: true,
        onSelect: () => {},
        onNewChat: () => {},
        onDelete: () => {},
        onCloseMobile: () => {},
        ...props,
      }))
    })
  }

  it('does not call onExpandArchived on initial render', () => {
    const onExpandArchived = vi.fn()
    renderList({ onExpandArchived })
    expect(onExpandArchived).not.toHaveBeenCalled()
  })

  it('calls onExpandArchived exactly once when the Archived section is expanded', () => {
    const onExpandArchived = vi.fn()
    renderList({ onExpandArchived })

    const btn = Array.from(host!.querySelectorAll('button')).find(b => b.textContent?.includes('Archived'))
    act(() => { btn!.dispatchEvent(new MouseEvent('click', { bubbles: true })) })

    expect(onExpandArchived).toHaveBeenCalledTimes(1)
  })

  it('does not call onExpandArchived again when collapsing', () => {
    const onExpandArchived = vi.fn()
    renderList({ onExpandArchived })

    const btn = Array.from(host!.querySelectorAll('button')).find(b => b.textContent?.includes('Archived'))
    act(() => { btn!.dispatchEvent(new MouseEvent('click', { bubbles: true })) }) // expand
    act(() => { btn!.dispatchEvent(new MouseEvent('click', { bubbles: true })) }) // collapse

    expect(onExpandArchived).toHaveBeenCalledTimes(1)
  })
})

