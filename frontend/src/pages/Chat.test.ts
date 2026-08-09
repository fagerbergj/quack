// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { liveDagFinalText, chatGitHubLink, EditableChatTitle, shouldQueueSubmit, mergeChatsPage, pollWhileVisible, pollPageExcludingPending, nextArchivedChats } from './Chat'
import type { DagTurnState } from '../state/chatStore'
import type { ChatSummary } from '../api'

// dag builds a minimal single-node DagTurnState (that node is the terminal node).
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

describe('shouldQueueSubmit - Composer send decision', () => {
  it('queues while the chat is streaming', () => {
    expect(shouldQueueSubmit(true)).toBe(true)
  })

  it('sends immediately when not streaming', () => {
    expect(shouldQueueSubmit(false)).toBe(false)
  })
})

// Regression: the answer bubble briefly showed the orchestrator's mid-processing
// narration (top-level `live.text`) before flipping to the terminal node's real
// answer once it arrived. The fix is that a DAG turn's answer text is ALWAYS the
// terminal node's nodeAnswer - orchestrator narration must never occupy it, even
// as a fallback while the node answer is still empty.
describe('liveDagFinalText - no mid-stream flip to orchestrator narration', () => {
  it("is empty while the terminal node's answer hasn't arrived yet, even with orchestrator narration present", () => {
    const d = dag({})
    expect(liveDagFinalText(d)).toBe('')
  })

  it("renders the terminal node's answer once set", () => {
    const d = dag({ a: 'the real answer' })
    expect(liveDagFinalText(d)).toBe('the real answer')
  })
})

// This mirrors the `liveText` selection in Chat.tsx: for a DAG turn it must be
// exactly liveDagFinalText(dag) - never `|| liveTopText`, which is what caused
// the flicker (narration shown, then replaced by the real answer).
describe('liveText selection (Chat.tsx local formula)', () => {
  function liveText(liveDag: DagTurnState | undefined, liveTopText: string): string {
    return liveDag ? liveDagFinalText(liveDag) : liveTopText
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

// #738 test 4: a hidden document does not issue poll requests; making it visible resumes
// polling (immediately, not on the next interval tick).
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

// Pins #382: the chat header exposes the originating GitHub PR/issue link for
// a GitHub-originated chat, and nothing for a direct (local) chat.
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

// EditableChatTitle drives the header's click-to-edit rename affordance
// (0.9.0): clicking the title swaps in an input; Enter/blur commits a real
// change via onRename (the caller wires this to api.renameChat + a store
// update), Escape cancels, and a blank/unchanged draft is a silent no-op -
// never a rename to an empty title.
describe('EditableChatTitle', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  // setInputValue goes through the native setter (bypassing React's value
  // tracker) so the subsequent 'input' event is seen as a real change -
  // setting .value directly is a no-op from React's perspective.
  function setInputValue(input: HTMLInputElement, value: string) {
    const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')!.set!
    setter.call(input, value)
    input.dispatchEvent(new Event('input', { bubbles: true }))
  }

  function renderTitle(props: { title: string; editable: boolean; onRename: (title: string) => void }) {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(EditableChatTitle, props))
    })
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
})

// #736 follow-up: the 5s sidebar poll must never shorten the chat list. A
// poll that re-requests "however many chats are loaded" comes back clamped
// to the server's page cap once the user has paged past it (109 chats on
// the live instance), and replacing the list with that shorter response
// silently drops the tail. The fix is a poll that only ever fetches the
// first page and merges it in, rather than requesting-and-replacing at the
// loaded count.
describe('mergeChatsPage - poll must never shorten the loaded list', () => {
  it('keeps every already-loaded chat when the polled page is smaller than what is loaded', () => {
    // Stands in for a sidebar paged past the server's page cap (100): the
    // poll's response (one page) is necessarily shorter than what's on screen.
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

  // Root cause: mergeChatsPage trusts `page` unconditionally (by design - that's
  // what lets a real status change win). A status=active poll GET that was in
  // flight when the user archived the open chat can still resolve with the
  // chat listed as active (server hadn't processed the PATCH yet when the GET
  // was served) - merged straight in, this undoes the optimistic removal. The
  // fix is pollPageExcludingPending, applied to the page before it ever reaches
  // mergeChatsPage - this is exactly what Chat.tsx's poll effect now does.
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

// #809 follow-up: archiving a chat before the Archived section has ever been expanded
// must not seed archivedChats - that flips it from undefined (unfetched) to a partial
// list, so handleExpandArchived's `archivedChats === undefined` check never fires and
// the section's real first page (which would include the newly archived chat) never loads.
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
