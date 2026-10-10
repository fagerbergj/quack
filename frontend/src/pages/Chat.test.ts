// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { createElement } from 'react'
import { act, cleanup, render } from '@testing-library/react'
import chatSrc from './Chat.tsx?raw'
import { chatGitHubLink, EditableChatTitle, mergeChatsPage, pollWhileVisible, pollPageExcludingPending, nextArchivedChats, resolveActiveChat } from './Chat'
import { dagAnswer, type DagTurnState } from '../state/chatStore'
import type { ChatSummary } from '../api'

afterEach(cleanup)

// Source-level guard (a full SSE + store + DAG harness is overkill for one prop): the live TriggerMessage
// must keep receiving chatId, or its <artifacts> rows go unclickable.
describe('live-turn TriggerMessage chatId wiring (#1252)', () => {
  it('passes chatId through so live-turn artifact rows can open the panel', () => {
    const idx = chatSrc.indexOf('<TriggerMessage')
    const call = chatSrc.slice(idx, idx + 400)
    expect(call).toMatch(/chatId=\{activeChatId/)
  })
})

// A run going active that this page isn't streaming always re-seeds and reattaches - a bare attach()
// lifted the chat's finished last turn into live and streamed the foreign run into it.
describe('foreign run on an open chat', () => {
  it('reattaches from a fresh GET whether or not the page has a live turn', () => {
    expect(chatSrc).toMatch(/if \(s !== 'running' \|\| store\.isStreaming\(activeChatId\)\) return\s+void api\.getChat\(activeChatId\)\.then\(detail => store\.reattach\(activeChatId, detail\.turns\)\)/)
  })
})

function dag(nodeAnswer: Record<string, string>): DagTurnState {
  return {
    planId: 'p',
    nodes: [{ id: 'a', agent: 'researcher', task: 't', depends_on: [] }],
    edges: [],
    nodeStates: {},
    nodeRuns: {},
    nodeAnswer,
  }
}

// A DAG turn's answer is always the terminal node's, never orchestrator narration, even while the node answer
// is still empty; the fallback made the bubble flicker.
describe('dagAnswer - no mid-stream flip to orchestrator narration', () => {
  it("is empty while the terminal node's answer hasn't arrived yet, even with orchestrator narration present", () => {
    const d = dag({})
    expect(dagAnswer(d).text).toBe('')
  })

  it("renders the terminal node's answer once set", () => {
    const d = dag({ a: 'the real answer' })
    expect(dagAnswer(d).text).toBe('the real answer')
  })

  it('streams every sink of a no-synthesizer plan as its own section, not the first sink alone', () => {
    const d: DagTurnState = {
      ...dag({ a: 'A', b: 'B' }),
      nodes: [{ id: 'a', agent: 'researcher', task: 't', depends_on: [] }, { id: 'b', agent: 'researcher', task: 'u', depends_on: [] }],
    }
    expect(dagAnswer(d).text).toBe('## a\n\nA\n\n## b\n\nB')
  })
})

// Mirrors Chat.tsx's `liveText` selection: never `|| liveTopText` for a DAG turn.
describe('liveText selection (Chat.tsx local formula)', () => {
  function liveText(liveDag: DagTurnState | undefined, liveTopText: string): string {
    return liveDag ? dagAnswer(liveDag).text : liveTopText
  }

  it('never shows orchestrator narration for a DAG turn, before or after the node answer arrives', () => {
    const narration = 'orchestrator is planning the request...'
    const midStream = dag({}) // terminal node answer still empty
    expect(liveText(midStream, narration)).toBe('')

    const settled = dag({ a: 'final answer' })
    expect(liveText(settled, narration)).toBe('final answer')
  })

  it('falls back to top-level text when there is no DAG at all (plain orchestrator reply)', () => {
    expect(liveText(undefined, 'a direct reply')).toBe('a direct reply')
  })
})

describe('pollWhileVisible - background-tab polling', () => {
  function setHidden(hidden: boolean) {
    Object.defineProperty(document, 'hidden', { value: hidden, configurable: true })
  }

  afterEach(() => {
    vi.useRealTimers()
    setHidden(false)
  })

  it('polls on the interval while the document is visible', () => {
    vi.useFakeTimers()
    setHidden(false)
    const poll = vi.fn()
    const stop = pollWhileVisible(poll, 1000)
    vi.advanceTimersByTime(3000)
    expect(poll).toHaveBeenCalledTimes(3)
    stop()
  })

  it('issues no poll requests while the document is hidden', () => {
    vi.useFakeTimers()
    setHidden(true)
    const poll = vi.fn()
    const stop = pollWhileVisible(poll, 1000)
    vi.advanceTimersByTime(5000)
    expect(poll).not.toHaveBeenCalled()
    stop()
  })

  it('resumes polling immediately when the document becomes visible again', () => {
    vi.useFakeTimers()
    setHidden(true)
    const poll = vi.fn()
    const stop = pollWhileVisible(poll, 1000)
    vi.advanceTimersByTime(2000)
    expect(poll).not.toHaveBeenCalled()

    setHidden(false)
    document.dispatchEvent(new Event('visibilitychange'))
    expect(poll).toHaveBeenCalledTimes(1)
    stop()
  })

  it('stops polling once the returned cleanup runs', () => {
    vi.useFakeTimers()
    setHidden(false)
    const poll = vi.fn()
    const stop = pollWhileVisible(poll, 1000)
    stop()
    vi.advanceTimersByTime(5000)
    expect(poll).not.toHaveBeenCalled()
  })
})

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

describe('chatGitHubLink', () => {
  it('exposes the url + repo for a GitHub-originated chat', () => {
    const c = chat({ id: 'github-acme-widgets-7', github_url: 'https://github.com/acme/widgets/issues/7', github_repo: 'acme/widgets' })
    expect(chatGitHubLink(c)).toEqual({ url: 'https://github.com/acme/widgets/issues/7', repo: 'acme/widgets' })
  })

  it('is null for a direct chat with no github_url', () => {
    expect(chatGitHubLink(chat({ id: 'a1b2c3' }))).toBeNull()
  })

  it('is null when there is no active chat at all', () => {
    expect(chatGitHubLink(undefined)).toBeNull()
  })
})

describe('EditableChatTitle', () => {
  let host: HTMLElement | undefined

  // Uses the native setter so React's value tracker sees the 'input' event as a real change.
  function setInputValue(input: HTMLInputElement, value: string) {
    const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')!.set!
    setter.call(input, value)
    input.dispatchEvent(new Event('input', { bubbles: true }))
  }

  function renderTitle(props: { title: string; editable: boolean; onRename: (title: string) => void }) {
    host = render(createElement(EditableChatTitle, props)).container
  }

  it('commits a renamed title on Enter', () => {
    const onRename = vi.fn()
    renderTitle({ title: 'Old title', editable: true, onRename })

    act(() => { host!.querySelector('h1')!.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
    const input = host!.querySelector('input')! as HTMLInputElement
    input.focus()
    act(() => { setInputValue(input, 'New title') })
    act(() => { input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })) })

    expect(onRename).toHaveBeenCalledWith('New title')
    expect(host!.querySelector('input')).toBeNull()
  })

  it('does not call onRename when the draft is unchanged or blank', () => {
    const onRename = vi.fn()
    renderTitle({ title: 'Old title', editable: true, onRename })

    act(() => { host!.querySelector('h1')!.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
    act(() => {
      const input = host!.querySelector('input')! as HTMLInputElement
      input.focus()
      input.blur()
    })
    expect(onRename).not.toHaveBeenCalled()

    act(() => { host!.querySelector('h1')!.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
    act(() => {
      const input = host!.querySelector('input')! as HTMLInputElement
      input.focus()
      setInputValue(input, '   ')
      input.blur()
    })
    expect(onRename).not.toHaveBeenCalled()
  })

  it('is not clickable (no rename affordance) when not editable', () => {
    const onRename = vi.fn()
    renderTitle({ title: 'Chat', editable: false, onRename })

    act(() => { host!.querySelector('h1')!.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
    expect(host!.querySelector('input')).toBeNull()
  })

  it('truncates to one line and keeps the full title as a tooltip', () => {
    const title = 'A very long chat title that would otherwise truncate'
    renderTitle({ title, editable: false, onRename: vi.fn() })
    const span = host!.querySelector('h1 span')!
    expect(span.className).toContain('truncate')
    expect(span.className).not.toContain('line-clamp')
    expect(span.getAttribute('title')).toBe(title)
  })
})

// A poll re-requesting the loaded count comes back clamped to the server's page cap, so replacing the list
// would drop the tail; the poll fetches the first page and merges it.
describe('mergeChatsPage - poll must never shorten the loaded list', () => {
  it('keeps every already-loaded chat when the polled page is smaller than what is loaded', () => {
    // A sidebar paged past the server's page cap (100).
    const existing = Array.from({ length: 109 }, (_, i) => chat({ id: `c${i}` }))
    const page = existing.slice(0, 20)

    const merged = mergeChatsPage(existing, page)

    expect(merged.length).toBeGreaterThanOrEqual(existing.length)
    for (const c of existing) {
      expect(merged.some(m => m.id === c.id)).toBe(true)
    }
  })

  it('updates a chat already in the list when it reappears in the polled page', () => {
    const stale = chat({ id: 'c1', status: 'idle' })
    const fresh = chat({ id: 'c1', status: 'running' })
    const merged = mergeChatsPage([stale, chat({ id: 'c2' })], [fresh])

    expect(merged).toHaveLength(2)
    expect(merged.find(c => c.id === 'c1')?.status).toBe('running')
  })

  it('adds a chat from the polled page that was not already loaded', () => {
    const merged = mergeChatsPage([chat({ id: 'c1' })], [chat({ id: 'c2' }), chat({ id: 'c1' })])
    expect(merged.map(c => c.id).sort()).toEqual(['c1', 'c2'])
  })

  // mergeChatsPage trusts `page` by design (a real status change must win), so a poll in flight during the
  // archive PATCH would undo the optimistic removal unless pollPageExcludingPending filters it first.
  it('a stale in-flight poll page no longer resurrects a chat just optimistically archived', () => {
    const afterOptimisticArchive = [chat({ id: 'c2' })] // c1 removed locally by handleArchiveChat
    const staleActivePage = [chat({ id: 'c1' }), chat({ id: 'c2' })] // server hadn't caught up yet
    const pendingArchiveIds = new Set(['c1'])
    const merged = mergeChatsPage(afterOptimisticArchive, pollPageExcludingPending(staleActivePage, pendingArchiveIds))
    expect(merged.some(c => c.id === 'c1')).toBe(false)
  })
})

describe('pollPageExcludingPending', () => {
  it('drops ids in pendingIds from the page', () => {
    const page = [chat({ id: 'c1' }), chat({ id: 'c2' })]
    expect(pollPageExcludingPending(page, new Set(['c1'])).map(c => c.id)).toEqual(['c2'])
  })

  it('is a no-op when pendingIds is empty', () => {
    const page = [chat({ id: 'c1' }), chat({ id: 'c2' })]
    expect(pollPageExcludingPending(page, new Set())).toBe(page)
  })
})

// Seeding an unfetched archivedChats would make handleExpandArchived's undefined check skip the real first page.
describe('nextArchivedChats - archive/unarchive transitions', () => {
  it('leaves archivedChats undefined when archiving before the section has ever loaded', () => {
    expect(nextArchivedChats(undefined, chat({ id: 'c1' }), true)).toBeUndefined()
  })

  it('prepends when archiving into an already-loaded list', () => {
    const result = nextArchivedChats([chat({ id: 'c2' })], chat({ id: 'c1' }), true)
    expect(result?.map(c => c.id)).toEqual(['c1', 'c2'])
  })

  it('removes the chat when unarchiving from a loaded list', () => {
    const result = nextArchivedChats([chat({ id: 'c1' }), chat({ id: 'c2' })], chat({ id: 'c1' }), false)
    expect(result?.map(c => c.id)).toEqual(['c2'])
  })

  it('is a no-op unarchiving against an unloaded (undefined) list', () => {
    expect(nextArchivedChats(undefined, chat({ id: 'c1' }), false)).toBeUndefined()
  })
})

// The header/composer read `archived` from here, so an archived chat must resolve though absent from `chats`.
describe('resolveActiveChat', () => {
  it('prefers the active-scoped `chats` list when the chat is there', () => {
    const inList = chat({ id: 'c1', title: 'from list' })
    const detail = chat({ id: 'c1', title: 'stale snapshot' })
    expect(resolveActiveChat([inList], 'c1', detail)?.title).toBe('from list')
  })

  it('falls back to the GetChat snapshot for a chat kept out of the list (archived)', () => {
    const detail = chat({ id: 'c1', archived: true })
    expect(resolveActiveChat([], 'c1', detail)).toEqual(detail)
  })

  it('is undefined when neither source matches the focused chat id', () => {
    expect(resolveActiveChat([chat({ id: 'other' })], 'c1', chat({ id: 'also-other' }))).toBeUndefined()
  })

  it('is undefined with no chat focused', () => {
    expect(resolveActiveChat([], null, null)).toBeUndefined()
  })
})
