// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import Plugins from './Plugins'
import { client } from '../generated/client.gen'

// Node's fetch/Request refuses to build a Request from a relative URL - see
// MemoryTab.test.ts for why baseUrl is set here.
client.setConfig({ baseUrl: 'http://localhost' })

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const ROW = {
  name: 'dotagents', entry: 'github:fagerbergj/dotagents', source: 'github' as const,
  installed_sha: 'c886ce1a8474939dc42f7c194f8c57242223ea1',
}

// Routes each fetch by method+path substring to a queue of canned responses,
// so a test only has to set up the endpoints it actually cares about -
// list/updates/create/delete/update all interleave in one page.
function routedFetch(routes: Record<string, Response[]>) {
  return vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url
    // The generated client calls fetch(new Request(...)) - method lives on
    // the Request, not a separate init object, for every non-GET call.
    const method = init?.method ?? (typeof input === 'object' && 'method' in input ? input.method : 'GET')
    for (const [key, queue] of Object.entries(routes)) {
      const [wantMethod, wantPath] = key.split(' ')
      if (method === wantMethod && url.includes(wantPath) && queue.length > 0) {
        return Promise.resolve(queue.shift()!)
      }
    }
    return Promise.reject(new Error(`unhandled ${method} ${url}`))
  })
}

describe('Plugins', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  beforeEach(() => {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    vi.stubGlobal('confirm', vi.fn(() => true))
  })

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
    vi.unstubAllGlobals()
  })

  async function renderAndFlush() {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    await act(async () => {
      root!.render(createElement(Plugins, { navOpen: false, onToggleNav: () => {} }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })
  }

  it('renders the list returned by GET /api/v1/plugins', async () => {
    vi.stubGlobal('fetch', routedFetch({
      'GET /plugins/updates': [jsonResponse({ updates: [] })],
      'GET /plugins': [jsonResponse({ plugins: [ROW] })],
    }))
    await renderAndFlush()
    expect(host!.textContent).toContain('dotagents')
    expect(host!.textContent).toContain('github')
  })

  it('shows an empty state when the list comes back empty', async () => {
    vi.stubGlobal('fetch', routedFetch({
      'GET /plugins/updates': [jsonResponse({ updates: [] })],
      'GET /plugins': [jsonResponse({ plugins: [] })],
    }))
    await renderAndFlush()
    expect(host!.textContent).toContain('No plugins registered')
  })

  it('shows a 400 message inline instead of throwing', async () => {
    vi.stubGlobal('fetch', routedFetch({
      'GET /plugins/updates': [jsonResponse({ updates: [] })],
      'GET /plugins': [jsonResponse({ plugins: [] })],
      'POST /plugins': [jsonResponse({ error: 'plugin entry "bad" does not match github:owner/repo[@ref][#path]' }, 400)],
    }))
    await renderAndFlush()

    const input = host!.querySelector('input[aria-label="Plugin entry"]') as HTMLInputElement
    const form = host!.querySelector('form') as HTMLFormElement
    // Goes through the native setter (bypassing React's value tracker) so the
    // 'input' event is seen as a real change - setting .value directly is a
    // no-op from React's perspective (see Chat.test.ts's setInputValue).
    const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')!.set!
    await act(async () => {
      setter.call(input, 'bad')
      input.dispatchEvent(new Event('input', { bubbles: true }))
      form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })
    expect(host!.textContent).toContain('github:owner/repo[@ref][#path]')
  })

  it('shows an "update available" badge for a behind row and clears it after Update', async () => {
    vi.stubGlobal('fetch', routedFetch({
      'GET /plugins/updates': [
        jsonResponse({ updates: [{ name: 'dotagents', behind: true, remote_sha: 'deadbeef' }] }),
        jsonResponse({ updates: [{ name: 'dotagents', behind: false, installed_sha: 'deadbeef' }] }),
      ],
      'GET /plugins': [
        jsonResponse({ plugins: [ROW] }),
        jsonResponse({ plugins: [{ ...ROW, installed_sha: 'deadbeef' }] }),
      ],
      'POST /plugins/dotagents/update': [jsonResponse({ ...ROW, installed_sha: 'deadbeef' })],
    }))
    await renderAndFlush()
    expect(host!.textContent).toContain('update available')

    const updateButton = host!.querySelector('button[aria-label="Update dotagents"]') as HTMLButtonElement
    await act(async () => {
      updateButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })
    expect(host!.textContent).not.toContain('update available')
  })

  it('issues DELETE only after the remove control is confirmed', async () => {
    vi.stubGlobal('fetch', routedFetch({
      'GET /plugins/updates': [jsonResponse({ updates: [] }), jsonResponse({ updates: [] })],
      'GET /plugins': [jsonResponse({ plugins: [ROW] }), jsonResponse({ plugins: [] })],
      'DELETE /plugins/dotagents': [new Response(null, { status: 204 })],
    }))
    await renderAndFlush()

    const removeButton = host!.querySelector('button[aria-label="Remove dotagents"]') as HTMLButtonElement
    await act(async () => {
      removeButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })
    expect(window.confirm).toHaveBeenCalled()
    expect(host!.textContent).toContain('No plugins registered')
  })
})
