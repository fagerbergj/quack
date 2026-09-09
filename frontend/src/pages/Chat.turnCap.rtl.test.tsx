// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { act } from 'react'
import { createRoot } from 'react-dom/client'
import Chat from './Chat'
import { ChatStore } from '../state/chatStore'
import { ChatStoreProvider } from '../state/ChatStoreProvider'
import { client } from '../generated/client.gen'
import type { Turn } from '../generated'

// Spy on TurnView itself (not its DOM output) so mount COUNT is cheap and
// exact to assert on, without paying for 500 real markdown parses (audit
// finding 9's own concern is the mount, not what's inside it).
const { mountedTurnIds } = vi.hoisted(() => ({ mountedTurnIds: [] as string[] }))
vi.mock('../components/TurnView', async importOriginal => {
  const actual = await importOriginal<typeof import('../components/TurnView')>()
  function Spy(props: { turn: { id: string } }) {
    mountedTurnIds.push(props.turn.id)
    return null
  }
  return { ...actual, TurnView: Spy }
})

const CHAT = { id: 'c1', title: 'cap test', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', archived: false, status: 'idle' }

function makeTurns(n: number): Turn[] {
  return Array.from({ length: n }, (_, i) => ({
    id: `t${i}`,
    created_at: '2026-01-01T00:00:00Z',
    input: { role: 'user' as const, content: `question number ${i}` },
    output: [{ id: `t${i}-m`, type: 'message' as const, status: 'completed' as const, content: [{ type: 'output_text' as const, text: 'a short answer' }] }],
  })) as unknown as Turn[]
}

// jsdom has no matchMedia; the theme hook calls it on mount.
function mockMatchMedia() {
  vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
    matches: false,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })))
}

function stubFetch(turns: Turn[]) {
  mockMatchMedia()
  client.setConfig({ baseUrl: 'http://localhost:3000' })
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = String(typeof input === 'string' ? input : input instanceof URL ? input.href : (input as Request).url)
    let body: unknown = { data: [] }
    if (/\/chats\/c1(\?|$)/.test(url)) body = { ...CHAT, turns, usage: { total_tokens: 0 } }
    else if (url.includes('/chats')) body = { data: [CHAT] }
    return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
  }))
}

describe('Chat turn list cap (audit finding 9)', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
    mountedTurnIds.length = 0
    vi.unstubAllGlobals()
  })

  async function mountChatWith(turns: Turn[]) {
    stubFetch(turns)
    window.history.replaceState(null, '', '/chat/c1')
    const store = new ChatStore()
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    await act(async () => {
      root!.render(
        <ChatStoreProvider store={store}><Chat navOpen={false} onToggleNav={() => {}} /></ChatStoreProvider>,
      )
    })
    // Let the async getChat().then(...) seed resolve.
    for (let i = 0; i < 50 && mountedTurnIds.length === 0; i++) {
      await act(async () => { await new Promise(r => setTimeout(r, 10)) })
    }
  }

  it('mounts only the most recent 100 TurnViews out of 500, with an older-messages control', async () => {
    await mountChatWith(makeTurns(500))

    // Chat re-renders more than once while it settles (poll/getChat effects),
    // so the same 100 ids get pushed more than once - the DISTINCT set is
    // what actually matters (that's what's on screen at any commit).
    let mounted = new Set(mountedTurnIds)
    expect(mounted.size).toBe(100)
    // The most recent 100 turns (t400..t499), not the first 100.
    expect(mounted.has('t400')).toBe(true)
    expect(mounted.has('t499')).toBe(true)
    expect(mounted.has('t399')).toBe(false)

    const button = Array.from(host!.querySelectorAll('button')).find(b => /older messages/.test(b.textContent ?? ''))
    expect(button).toBeDefined()
    expect(button!.textContent).toContain('100 older messages')

    mountedTurnIds.length = 0
    await act(async () => { button!.click() })
    mounted = new Set(mountedTurnIds)
    expect(mounted.size).toBe(200) // 100 more revealed, capped set re-renders in full
    expect(mounted.has('t300')).toBe(true)
    expect(mounted.has('t499')).toBe(true)
    expect(mounted.has('t299')).toBe(false)
  })

  it('mounts every turn (no control) when the chat has fewer than the cap', async () => {
    await mountChatWith(makeTurns(40))

    expect(new Set(mountedTurnIds).size).toBe(40)
    const button = Array.from(host!.querySelectorAll('button')).find(b => /older messages/.test(b.textContent ?? ''))
    expect(button).toBeUndefined()
  })
})
