// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { liveDagFinalText, chatGitHubLink, EditableChatTitle, shouldQueueSubmit } from './Chat'
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
