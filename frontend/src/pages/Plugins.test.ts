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

const emptyReload = {
  generation: 2,
  agents: { added: [], updated: [], removed: [] },
  workflows: { added: [], updated: [], removed: [] },
  mcp_servers: { started: [], reused: [], stopped: [] },
  failures: [],
}

const changedReload = {
  generation: 3,
  agents: { added: ['sleeper-analyst'], updated: [{ name: 'code-reviewer', bundle_hash: 'abcdef1234567890' }], removed: ['legacy-planner'] },
  workflows: { added: ['weekly-digest'], updated: [], removed: [] },
  mcp_servers: { started: ['ponytail/lint'], reused: ['dotagents/search'], stopped: [] },
  failures: [],
}

const ROW = {
  name: 'dotagents', entry: 'github:fagerbergj/dotagents', source: 'github' as const,
  installed_sha: 'c886ce1a8474939dc42f7c194f8c57242223ea1',
}

const ROW2 = {
  name: 'ponytail', entry: 'github:fagerbergj/ponytail', source: 'github' as const,
  installed_sha: '0a4dd63ad4541f4f655c4108a295916f3c1d8fd',
}

// Routes each fetch by method+path substring to a queue of canned responses,
// so a test only has to set up the endpoints it actually cares about -
// list/updates/create/delete/update all interleave in one page.
function routedFetch(routes: Record<string, Array<Response | Promise<Response>>>) {
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
      'DELETE /plugins/dotagents': [jsonResponse({ reload: emptyReload })],
    }))
    await renderAndFlush()

    const removeButton = host!.querySelector('button[aria-label="Remove dotagents"]') as HTMLButtonElement
    await act(async () => {
      removeButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })
    expect(window.confirm).toHaveBeenCalled()
    expect(host!.textContent).toContain('No plugins registered')
    expect(host!.textContent).toContain('Reloaded - generation 2')
    expect(host!.textContent).toContain('No changes')
  })

  it('sends no DELETE when the remove confirm is declined', async () => {
    vi.stubGlobal('confirm', vi.fn(() => false))
    const fetchMock = routedFetch({
      'GET /plugins/updates': [jsonResponse({ updates: [] })],
      'GET /plugins': [jsonResponse({ plugins: [ROW] })],
    })
    vi.stubGlobal('fetch', fetchMock)
    await renderAndFlush()

    const removeButton = host!.querySelector('button[aria-label="Remove dotagents"]') as HTMLButtonElement
    await act(async () => {
      removeButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })
    expect(window.confirm).toHaveBeenCalled()
    expect(host!.textContent).toContain('dotagents')
    expect(fetchMock.mock.calls.some(([input]) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : (input as Request).url
      return url.includes('/plugins/dotagents') && !url.includes('/update')
    })).toBe(false)
  })

  // severe#3 regression: a failed action must not blank the list that
  // already loaded fine.
  it('shows a DELETE failure as a banner without hiding the other rows', async () => {
    vi.stubGlobal('fetch', routedFetch({
      'GET /plugins/updates': [jsonResponse({ updates: [] })],
      'GET /plugins': [jsonResponse({ plugins: [ROW, ROW2] })],
      'DELETE /plugins/dotagents': [jsonResponse({ error: 'boom' }, 500)],
    }))
    await renderAndFlush()

    const removeButton = host!.querySelector('button[aria-label="Remove dotagents"]') as HTMLButtonElement
    await act(async () => {
      removeButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })
    expect(host!.textContent).toContain('dotagents')
    expect(host!.textContent).toContain('ponytail')
    expect(host!.textContent).toContain('boom')
  })

  // PR #1442 carry-over: DELETE succeeds but the silent post-action refresh
  // then fails - the rows on screen must survive, surfaced as actionError.
  it('keeps the rows and shows actionError when a silent post-action refresh fails', async () => {
    vi.stubGlobal('fetch', routedFetch({
      'GET /plugins/updates': [jsonResponse({ updates: [] })],
      'GET /plugins': [
        jsonResponse({ plugins: [ROW, ROW2] }),
        jsonResponse({ error: 'boom' }, 500),
      ],
      'DELETE /plugins/dotagents': [jsonResponse({ reload: emptyReload })],
    }))
    await renderAndFlush()
    expect(host!.textContent).toContain('dotagents')
    expect(host!.textContent).toContain('ponytail')

    const removeButton = host!.querySelector('button[aria-label="Remove dotagents"]') as HTMLButtonElement
    await act(async () => {
      removeButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })

    // The DELETE succeeded, but the refresh it triggered failed - the rows
    // from the first, successful GET must still be on screen.
    expect(host!.textContent).toContain('dotagents')
    expect(host!.textContent).toContain('ponytail')
    expect(host!.textContent).toContain('boom')
  })

  describe('reload report', () => {
    const tick = () => new Promise(resolve => setTimeout(resolve, 0))
    const panel = () => host!.querySelector('[aria-live="polite"]')!.textContent ?? ''
    const button = (name: string) =>
      [...host!.querySelectorAll('button')].find(b => b.textContent?.trim() === name || b.getAttribute('aria-label') === name) as HTMLButtonElement

    async function click(name: string) {
      await act(async () => {
        button(name).dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
        await tick()
      })
    }

    const listTwice = () => ({
      'GET /plugins/updates': [jsonResponse({ updates: [] }), jsonResponse({ updates: [] })],
      'GET /plugins': [jsonResponse({ plugins: [ROW] }), jsonResponse({ plugins: [ROW] })],
    })

    it('disables Reload while in flight, then shows what changed', async () => {
      let release!: (r: Response) => void
      const pending = new Promise<Response>(resolve => { release = resolve })
      vi.stubGlobal('fetch', routedFetch({ ...listTwice(), 'POST /plugins/reload': [pending] }))
      await renderAndFlush()

      await click('Reload')
      expect(button('Reload').disabled).toBe(true)
      await act(async () => { release(jsonResponse(changedReload)); await tick() })

      expect(button('Reload').disabled).toBe(false)
      for (const want of ['Reloaded - generation 3', 'sleeper-analyst', 'code-reviewer (abcdef1)', 'legacy-planner', 'weekly-digest', 'ponytail/lint', 'dotagents/search']) {
        expect(panel()).toContain(want)
      }
    })

    it('lists dropped members as warnings', async () => {
      const report = {
        ...changedReload,
        failures: [
          { plugin: 'ponytail', member: 'lint', stage: 'mcp', error: 'spawn ponytail-lint: not found' },
          { stage: 'config', error: 'agent "x": unknown model' },
        ],
      }
      vi.stubGlobal('fetch', routedFetch({ ...listTwice(), 'POST /plugins/reload': [jsonResponse(report)] }))
      await renderAndFlush()
      await click('Reload')

      expect(panel()).toContain('ponytail/lint (mcp): spawn ponytail-lint: not found')
      expect(panel()).toContain('reload (config): agent "x": unknown model')
      const list = host!.querySelector('section[aria-label="Reload report"] ul')!
      expect(list.className).toContain('text-amber-700')
    })

    it('shows a 422 as aborted on the still-serving generation, and dismisses', async () => {
      const report = { ...emptyReload, failures: [{ stage: 'registry', error: 'list plugins: connection refused' }] }
      vi.stubGlobal('fetch', routedFetch({ ...listTwice(), 'POST /plugins/reload': [jsonResponse(report, 422)] }))
      await renderAndFlush()
      await click('Reload')

      expect(panel()).toContain('Reload aborted - still serving generation 2')
      expect(panel()).toContain('list plugins: connection refused')
      await click('Dismiss reload report')
      expect(panel()).toBe('')
    })

    it('shows the reload an Update returned without a separate Reload', async () => {
      vi.stubGlobal('fetch', routedFetch({ ...listTwice(), 'POST /plugins/dotagents/update': [jsonResponse({ ...ROW, reload: changedReload })] }))
      await renderAndFlush()
      await click('Update dotagents')
      expect(panel()).toContain('Reloaded - generation 3')
      expect(panel()).toContain('sleeper-analyst')
    })

    it('shows both the message and the report when an Update is refused with a 422', async () => {
      const report = { ...emptyReload, failures: [{ plugin: 'dotagents', stage: 'admission', error: 'declares unlinked module' }] }
      vi.stubGlobal('fetch', routedFetch({
        ...listTwice(),
        'POST /plugins/dotagents/update': [jsonResponse({ error: 'plugin dotagents refused', reload: report }, 422)],
      }))
      await renderAndFlush()
      await click('Update dotagents')
      expect(host!.textContent).toContain('plugin dotagents refused')
      expect(panel()).toContain('dotagents (admission): declares unlinked module')
    })

    it('shows the reload an Add returned', async () => {
      vi.stubGlobal('fetch', routedFetch({ ...listTwice(), 'POST /plugins': [jsonResponse({ ...ROW, reload: changedReload }, 201)] }))
      await renderAndFlush()
      const input = host!.querySelector('input[aria-label="Plugin entry"]') as HTMLInputElement
      const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')!.set!
      await act(async () => {
        setter.call(input, 'github:fagerbergj/dotagents')
        input.dispatchEvent(new Event('input', { bubbles: true }))
        host!.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
        await tick()
      })
      expect(panel()).toContain('Reloaded - generation 3')
    })
  })
})
