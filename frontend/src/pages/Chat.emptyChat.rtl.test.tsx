// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { act } from 'react'
import { createRoot } from 'react-dom/client'
import Chat from './Chat'
import { ChatStore } from '../state/chatStore'
import { ChatStoreProvider } from '../state/ChatStoreProvider'
import { client } from '../generated/client.gen'
import { api, type ChatSummary } from '../api'

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

// mountEmptyChat renders Chat at the bare /chat route with the given store
// and returns the composer's textarea plus a helper to type text and press
// Enter (two separate act() flushes, like userEvent, so React's state update
// from the `input` event lands before the `keydown` handler reads it).
async function mountEmptyChat(host: HTMLDivElement, store: ChatStore) {
  const root = createRoot(host)
  // @ts-expect-error react act environment flag
  globalThis.IS_REACT_ACT_ENVIRONMENT = true
  await act(async () => {
    root.render(
      <ChatStoreProvider store={store}><Chat navOpen={false} onToggleNav={() => {}} /></ChatStoreProvider>,
    )
  })
  await act(async () => { await new Promise(r => setTimeout(r, 10)) }) // let loadChats() resolve
  const textarea = host.querySelector('textarea') as HTMLTextAreaElement
  const valueSetter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!
  async function sendMessage(text: string) {
    await act(async () => {
      valueSetter.call(textarea, text)
      textarea.dispatchEvent(new Event('input', { bubbles: true }))
    })
    await act(async () => {
      textarea.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }))
      await new Promise(r => setTimeout(r, 0))
    })
  }
  return { root, textarea, sendMessage }
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
    const mounted = await mountEmptyChat(host, store)
    root = mounted.root

    expect(host.textContent).not.toContain('Select or start a chat')
    expect(mounted.textarea.disabled).toBe(false)
    expect(mounted.textarea.placeholder).toBe('Ask a question')

    await mounted.sendMessage('hello there')

    expect(api.createChat).toHaveBeenCalledTimes(1)
    expect(window.location.pathname).toBe('/chat/new-chat')
    expect(submitSpy).toHaveBeenCalledWith('new-chat', 'hello there', undefined, expect.any(Function))
  })

  // Review finding: createChat rejecting used to erase the typed draft,
  // show nothing, and leave an unhandled rejection - it must restore the
  // draft and surface a visible error instead.
  it('a failed create restores the draft and shows an error', async () => {
    mockMatchMedia()
    client.setConfig({ baseUrl: 'http://localhost:3000' })
    window.history.replaceState(null, '', '/chat')
    vi.mocked(api.createChat).mockRejectedValueOnce(new Error('network down'))

    const store = new ChatStore()
    host = document.createElement('div')
    document.body.appendChild(host)
    const mounted = await mountEmptyChat(host, store)
    root = mounted.root

    await mounted.sendMessage('hello there')

    expect(api.createChat).toHaveBeenCalledTimes(1)
    expect(mounted.textarea.value).toBe('hello there') // draft restored, not lost
    expect(host.textContent).toContain('network down') // surfaced, not silent
  })

  // Review finding: two sends before createChat resolves both saw
  // activeChatId null and would each create their own chat. A single
  // in-flight create promise must be shared, so exactly one chat gets
  // created and both messages land in it.
  it('two rapid sends before createChat resolves create exactly one chat', async () => {
    mockMatchMedia()
    client.setConfig({ baseUrl: 'http://localhost:3000' })
    window.history.replaceState(null, '', '/chat')
    let resolveCreate!: (chat: ChatSummary) => void
    vi.mocked(api.createChat).mockImplementationOnce(() => new Promise(res => { resolveCreate = res }))

    const store = new ChatStore()
    const submitSpy = vi.spyOn(store, 'submit').mockResolvedValue(undefined)
    host = document.createElement('div')
    document.body.appendChild(host)
    const mounted = await mountEmptyChat(host, store)
    root = mounted.root

    await mounted.sendMessage('first message')
    expect(api.createChat).toHaveBeenCalledTimes(1) // first send kicked off the (still-pending) create

    await mounted.sendMessage('second message')
    expect(api.createChat).toHaveBeenCalledTimes(1) // second send reused it, didn't start a new one

    await act(async () => {
      resolveCreate({ id: 'new-chat', title: 'Untitled chat', system_prompt: '', created_at: NOW, updated_at: NOW, status: 'idle' })
      await new Promise(r => setTimeout(r, 10))
    })

    expect(window.location.pathname).toBe('/chat/new-chat')
    expect(submitSpy).toHaveBeenCalledTimes(2)
    expect(submitSpy.mock.calls[0].slice(0, 2)).toEqual(['new-chat', 'first message'])
    expect(submitSpy.mock.calls[1].slice(0, 2)).toEqual(['new-chat', 'second message'])
  })
})
