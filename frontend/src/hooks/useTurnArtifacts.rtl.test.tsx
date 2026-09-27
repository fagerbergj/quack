// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { useTurnArtifacts } from './useTurnArtifacts'
import { client } from '../generated/client.gen'
import { EMPTY_STATE, type ChatState } from '../state/chatStore'

afterEach(() => vi.unstubAllGlobals())

describe('useTurnArtifacts', () => {
  it('places a surface with no turn_id or pin on a live turn lifted back in by attach, by created_at', async () => {
    client.setConfig({ baseUrl: 'http://localhost' })
    const list = { data: [{ name: 'a2ui_surface:s1', kind: 'a2ui_surface', revisions: [{ revision: 1, mime_type: 'application/json', size: 1, created_at: '2026-09-27T10:02:00Z' }] }] }
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(list), { headers: { 'Content-Type': 'application/json' } })))
    const state: ChatState = {
      ...EMPTY_STATE,
      turns: [{ id: 't1', created_at: '2026-09-27T09:00:00Z', input: { role: 'user', content: 'a' }, output: [] }],
      live: { id: 't2', createdAt: '2026-09-27T10:00:00Z', userText: 'b', streaming: false, error: '', text: '', runs: [] },
    }
    const { result } = renderHook(() => useTurnArtifacts('chat-1', state))
    await waitFor(() => expect(result.current.turnSurfaces).toEqual({ t2: [{ name: 'a2ui_surface:s1', revision: 1 }] }))
  })
})
