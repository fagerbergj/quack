// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { act, cleanup, render } from '@testing-library/react'
import Chat from './Chat'
import { ChatStore } from '../state/chatStore'
import { ChatStoreProvider } from '../state/ChatStoreProvider'
import { client } from '../generated/client.gen'
import { api, type ChatSummary } from '../api'

afterEach(cleanup)

const NOW = '2026-01-01T00:00:00Z'

// api is mocked rather than fetch because submitMessage calls api directly.
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

// Two separate act() flushes, like userEvent, so the `input` state update lands before `keydown` reads it.
async function mountEmptyChat(store: ChatStore) {
  const host = render(<ChatStoreProvider store={store}><Chat navOpen={false} onToggleNav={() => {}} /></ChatStoreProvider>).container
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
  return { host, textarea, sendMessage }
}

describe('Chat empty /chat route (audit finding 8)', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.clearAllMocks()
  })

  it('composer is enabled with "Ask a question"; first send creates the chat and submits into it', async () => {
    mockMatchMedia()
    client.setConfig({ baseUrl: 'http://localhost:3000' })
    window.history.replaceState(null, '', '/chat')

    const store = new ChatStore()
    const submitSpy = vi.spyOn(store, 'submit').mockResolvedValue(undefined)

    const mounted = await mountEmptyChat(store)
    const host = mounted.host

    expect(host.textContent).not.toContain('Select or start a chat')
    expect(mounted.textarea.disabled).toBe(false)
    expect(mounted.textarea.placeholder).toBe('Ask a question')

    await mounted.sendMessage('hello there')

    expect(api.createChat).toHaveBeenCalledTimes(1)
    expect(window.location.pathname).toBe('/chat/new-chat')
    expect(submitSpy).toHaveBeenCalledWith('new-chat', 'hello there', undefined, expect.any(Function))
  })

  it('a failed create restores the draft and shows an error', async () => {
    mockMatchMedia()
    client.setConfig({ baseUrl: 'http://localhost:3000' })
    window.history.replaceState(null, '', '/chat')
    vi.mocked(api.createChat).mockRejectedValueOnce(new Error('network down'))

    const store = new ChatStore()
    const mounted = await mountEmptyChat(store)
    const host = mounted.host

    await mounted.sendMessage('hello there')

    expect(api.createChat).toHaveBeenCalledTimes(1)
    expect(mounted.textarea.value).toBe('hello there') // draft restored, not lost
    expect(host.textContent).toContain('network down') // surfaced, not silent
  })

  // Both sends see activeChatId null before createChat resolves; the shared promise must yield one chat.
  it('two rapid sends before createChat resolves create exactly one chat', async () => {
    mockMatchMedia()
    client.setConfig({ baseUrl: 'http://localhost:3000' })
    window.history.replaceState(null, '', '/chat')
    let resolveCreate!: (chat: ChatSummary) => void
    vi.mocked(api.createChat).mockImplementationOnce(() => new Promise(res => { resolveCreate = res }))

    const store = new ChatStore()
    const submitSpy = vi.spyOn(store, 'submit').mockResolvedValue(undefined)
    const mounted = await mountEmptyChat(store)

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
