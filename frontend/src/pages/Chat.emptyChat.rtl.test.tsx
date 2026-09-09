// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { act } from 'react'
import { createRoot } from 'react-dom/client'
import Chat from './Chat'
import { ChatStore } from '../state/chatStore'
import { ChatStoreProvider } from '../state/ChatStoreProvider'
import { client } from '../generated/client.gen'
import { api } from '../api'

const NOW = '2026-01-01T00:00:00Z'

// Audit finding 8: the empty /chat route used to disable the composer and
// show a "Select or start a chat" label with a redundant New Chat button.
// api.createChat/listChats are mocked (not fetch) since submitMessage calls
// api directly, same seam as Chat.turnCap.rtl.test.tsx's fetch stub.
vi.mock('../api', async importOriginal => {
  const actual = await importOriginal<typeof import('../api')>()
  return {
    ...actual,
    api: {
      ...actual.api,
      listChats: vi.fn(async () => ({ data: [] })),
      createChat: vi.fn(async () => ({
        id: 'new-chat',
        title: 'Untitled chat',
        system_prompt: '',
        created_at: NOW,
        updated_at: NOW,
        status: 'idle',
      })),
    },
  }
})

function mockMatchMedia() {
  vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
    matches: false,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })))
}

describe('Chat empty /chat route (audit finding 8)', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
    vi.unstubAllGlobals()
    vi.clearAllMocks()
  })

  it('composer is enabled with "Ask a question"; first send creates the chat and submits into it', async () => {
    mockMatchMedia()
    client.setConfig({ baseUrl: 'http://localhost:3000' })
    window.history.replaceState(null, '', '/chat')

    const store = new ChatStore()
    const submitSpy = vi.spyOn(store, 'submit').mockResolvedValue(undefined)

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
    await act(async () => { await new Promise(r => setTimeout(r, 10)) }) // let loadChats() resolve

    expect(host.textContent).not.toContain('Select or start a chat')
    const textarea = host.querySelector('textarea') as HTMLTextAreaElement
    expect(textarea).not.toBeNull()
    expect(textarea.disabled).toBe(false)
    expect(textarea.placeholder).toBe('Ask a question')

    const valueSetter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!
    await act(async () => {
      valueSetter.call(textarea, 'hello there')
      textarea.dispatchEvent(new Event('input', { bubbles: true }))
    })
    await act(async () => {
      textarea.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }))
      await new Promise(r => setTimeout(r, 10))
    })

    expect(api.createChat).toHaveBeenCalledTimes(1)
    expect(window.location.pathname).toBe('/chat/new-chat')
    expect(submitSpy).toHaveBeenCalledWith('new-chat', 'hello there', undefined, expect.any(Function))
  })
})
