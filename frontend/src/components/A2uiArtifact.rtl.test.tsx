// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { TurnSurfaces } from './A2uiArtifact'
import { ChatStoreProvider } from '../state/ChatStoreProvider'
import { ChatStore } from '../state/chatStore'

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

const surface = {
  surface_id: 's1',
  components: [
    { id: 'root', component: 'Button', child: 'label', action: { event: { name: 'go', context: { pick: { path: '/pick' } } } } },
    { id: 'label', component: 'Text', text: 'Go' },
  ],
  data_model: { pick: 'a' },
}

describe('TurnSurfaces', () => {
  it('fetches the latest revision and sends a button press as a chat action turn', async () => {
    const fetchMock = vi.fn(async () => new Response(JSON.stringify(surface)))
    vi.stubGlobal('fetch', fetchMock)
    const store = new ChatStore()
    const submit = vi.spyOn(store, 'submitA2uiAction').mockResolvedValue()
    render(<ChatStoreProvider store={store}><TurnSurfaces chatId="c1" surfaces={[{ name: 'a2ui_surface:s1', revision: 3 }]} /></ChatStoreProvider>)
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Go' }, { timeout: 20000 }))
    expect(fetchMock).toHaveBeenCalledWith('/api/v1/chats/c1/artifacts/a2ui_surface%3As1?revision=3')
    expect(submit).toHaveBeenCalledWith('c1', { surface_id: 's1', name: 'go', source_component_id: 'root', context: { pick: 'a' } })
  }, 30000)

  it('renders nothing without surfaces', () => {
    const { container } = render(<TurnSurfaces chatId="c1" surfaces={[]} />)
    expect(container.innerHTML).toBe('')
  })
})
