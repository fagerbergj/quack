import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ChatStore } from '../state/chatStore'

// Minimal SSE stream that returns a done event so the store doesn't hang.
function makeStream(body: string): Response {
  const encoder = new TextEncoder()
  const stream = new ReadableStream({
    start(ctrl) {
      ctrl.enqueue(encoder.encode(body))
      ctrl.close()
    },
  })
  return new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

describe('ChatStore.submit — attachment transport', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  let store: ChatStore

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    store = new ChatStore()
    // Seed a fake chat so submit can proceed without an active chat guard.
    store.seed('chat-1', [])
  })

  it('sends JSON when no files are attached', async () => {
    fetchMock.mockResolvedValue(makeStream(''))
    await store.submit('chat-1', 'hello')
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(init.headers).toBeDefined()
    expect((init.headers as Record<string, string>)['Content-Type']).toBe('application/json')
    const body = JSON.parse(init.body as string)
    expect(body.content).toBe('hello')
  })

  it('sends FormData with files when attachments are provided', async () => {
    fetchMock.mockResolvedValue(makeStream(''))
    const file = new File(['data'], 'photo.jpg', { type: 'image/jpeg' })
    await store.submit('chat-1', 'what is in this image?', [file])
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(init.body).toBeInstanceOf(FormData)
    const fd = init.body as FormData
    expect(fd.get('content')).toBe('what is in this image?')
    const files = fd.getAll('files')
    expect(files).toHaveLength(1)
    expect((files[0] as File).name).toBe('photo.jpg')
    expect((files[0] as File).type).toBe('image/jpeg')
  })

  it('sends FormData with multiple files', async () => {
    fetchMock.mockResolvedValue(makeStream(''))
    const img = new File(['img'], 'shot.png', { type: 'image/png' })
    const audio = new File(['audio'], 'clip.wav', { type: 'audio/wav' })
    await store.submit('chat-1', 'describe these', [img, audio])
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    const fd = init.body as FormData
    const files = fd.getAll('files')
    expect(files).toHaveLength(2)
    expect((files[0] as File).name).toBe('shot.png')
    expect((files[1] as File).name).toBe('clip.wav')
  })

  it('falls back to JSON when files array is empty', async () => {
    fetchMock.mockResolvedValue(makeStream(''))
    await store.submit('chat-1', 'no files', [])
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(init.body).toBeTypeOf('string')
    expect(JSON.parse(init.body as string).content).toBe('no files')
  })
})
