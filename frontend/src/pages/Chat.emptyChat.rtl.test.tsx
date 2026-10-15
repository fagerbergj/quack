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
      getChat: vi.fn(async (id: string) => ({ id, title: '', system_prompt: '', created_at: NOW, updated_at: NOW, status: 'idle', turns: [] })),
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

  // Dogfood finding 11: every click used to POST /chats and leave an empty chat behind.
  it('New Chat creates nothing until the first send, and an untitled chat reads "(untitled)"', async () => {
    mockMatchMedia()
    client.setConfig({ baseUrl: 'http://localhost:3000' })
    window.history.replaceState(null, '', '/chat/old')
    vi.mocked(api.listChats).mockResolvedValue({ data: [{ id: 'old', title: '', system_prompt: '', created_at: NOW, updated_at: NOW, status: 'idle' }] })

    const store = new ChatStore()
    const submitSpy = vi.spyOn(store, 'submit').mockResolvedValue(undefined)
    const mounted = await mountEmptyChat(store)
    expect(mounted.host.querySelector('h1')?.textContent).toBe('(untitled)')

    const newChat = [...mounted.host.querySelectorAll('button')].find(b => b.textContent === 'New Chat')!
    await act(async () => { newChat.click() })
    await act(async () => { newChat.click() })
    expect(api.createChat).not.toHaveBeenCalled()
    expect(window.location.pathname).toBe('/chat')
    expect(mounted.host.querySelector('h1')?.textContent).toBe('New chat')

    await mounted.sendMessage('hello')
    expect(api.createChat).toHaveBeenCalledTimes(1)
    expect(submitSpy).toHaveBeenCalledWith('new-chat', 'hello', undefined, expect.any(Function))
    vi.mocked(api.listChats).mockResolvedValue({ data: [] })
  })

  // Dogfood finding 12: a bad chat URL showed a working composer, then dropped the message with "no such chat".
  it('a chat URL that 404s shows not-found, and Start a new chat keeps the typed draft', async () => {
    mockMatchMedia()
    client.setConfig({ baseUrl: 'http://localhost:3000' })
    window.history.replaceState(null, '', '/chat/does-not-exist')
    vi.mocked(api.getChat).mockRejectedValueOnce(Object.assign(new Error('no such chat'), { status: 404 }))

    const store = new ChatStore()
    const submitSpy = vi.spyOn(store, 'submit').mockResolvedValue(undefined)
    const mounted = await mountEmptyChat(store)
    expect(mounted.host.textContent).toContain("This chat doesn't exist or was deleted.")
    expect(mounted.host.querySelector('h1')?.textContent).toBe('Chat not found')

    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(mounted.textarea, 'my draft')
      mounted.textarea.dispatchEvent(new Event('input', { bubbles: true }))
    })
    const start = [...mounted.host.querySelectorAll('button')].find(b => b.textContent === 'Start a new chat')!
    await act(async () => { start.click() })
    expect(window.location.pathname).toBe('/chat')
    expect(mounted.textarea.value).toBe('my draft')
    expect(api.createChat).not.toHaveBeenCalled()

    await mounted.sendMessage('my draft')
    expect(submitSpy).toHaveBeenCalledWith('new-chat', 'my draft', undefined, expect.any(Function))
  })

  it('Enter on a not-found chat sends into a new chat, not the missing one', async () => {
    mockMatchMedia()
    client.setConfig({ baseUrl: 'http://localhost:3000' })
    window.history.replaceState(null, '', '/chat/does-not-exist')
    vi.mocked(api.getChat).mockRejectedValueOnce(Object.assign(new Error('no such chat'), { status: 404 }))

    const store = new ChatStore()
    const submitSpy = vi.spyOn(store, 'submit').mockResolvedValue(undefined)
    const mounted = await mountEmptyChat(store)
    await mounted.sendMessage('hello')
    expect(api.createChat).toHaveBeenCalledTimes(1)
    expect(submitSpy).toHaveBeenCalledWith('new-chat', 'hello', undefined, expect.any(Function))
    expect(window.location.pathname).toBe('/chat/new-chat')
  })

  it('says "Loading chat…", not "Ask a question", until an existing chat\'s history arrives', async () => {
    mockMatchMedia()
    client.setConfig({ baseUrl: 'http://localhost:3000' })
    window.history.replaceState(null, '', '/chat/long-chat')
    let resolve!: (v: Awaited<ReturnType<typeof api.getChat>>) => void
    vi.mocked(api.getChat).mockImplementationOnce(() => new Promise(r => { resolve = r }))

    const { host } = await mountEmptyChat(new ChatStore())
    expect(host.querySelector('[role=status]')?.textContent).toBe('Loading chat…')
    expect(host.textContent).not.toContain('Ask a question')

    await act(async () => { resolve({ id: 'long-chat', title: '', system_prompt: '', created_at: NOW, updated_at: NOW, status: 'idle', turns: [], usage: {} }) })
    expect(host.textContent).not.toContain('Loading chat…')
    expect(host.textContent).toContain('Ask a question')
  })
})
