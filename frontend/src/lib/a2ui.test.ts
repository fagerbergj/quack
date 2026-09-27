import { describe, it, expect } from 'vitest'
import { a2uiActionText, keepLocalEdits, parseA2uiActionText, restoredPicks, surfaceMessages, surfacesByTurn, QUACK_CATALOG_ID, type SurfaceContent } from './a2ui'
import type { ArtifactList } from '../generated'

const v1: SurfaceContent = {
  surface_id: 's1',
  components: [{ id: 'root', component: 'Text', text: 'hi' }],
  data_model: { answers: { q1: [], q2: [] } },
}

describe('surfaceMessages', () => {
  it('creates the surface, then components, then the data model on first render', () => {
    expect(surfaceMessages(undefined, v1, undefined)).toEqual([
      { version: 'v0.9.1', createSurface: { surfaceId: 's1', catalogId: QUACK_CATALOG_ID } },
      { version: 'v0.9.1', updateComponents: { surfaceId: 's1', components: v1.components } },
      { version: 'v0.9.1', updateDataModel: { surfaceId: 's1', path: '/', value: v1.data_model } },
    ])
  })

  it('sends only components when the stored data model is unchanged', () => {
    const v2 = { ...v1, components: [{ id: 'root', component: 'Text', text: 'graded' }] }
    expect(surfaceMessages(v1, v2, { answers: { q1: ['a'] } })).toEqual([
      { version: 'v0.9.1', updateComponents: { surfaceId: 's1', components: v2.components } },
    ])
  })

  it('merges a changed data model over the user edits whose paths still exist', () => {
    const v2 = { ...v1, data_model: { answers: { q1: [], q3: [] }, score: 0 } }
    const [, dm] = surfaceMessages(v1, v2, { answers: { q1: ['a'], q2: ['c'] } })
    expect(dm).toEqual({ version: 'v0.9.1', updateDataModel: { surfaceId: 's1', path: '/', value: { answers: { q1: ['a'], q3: [] }, score: 0 } } })
  })
})

describe('surfaceMessages question replacement', () => {
  const picker = (label: string) => ({ id: 'q1', component: 'ChoicePicker', label, options: [{ label: 'x', value: 'a' }], value: { path: '/answers/q1' } })
  it('resets the answer path of a ChoicePicker whose question changed, even with an unchanged data model', () => {
    const a = { ...v1, components: [picker('Old?')] }
    const b = { ...v1, components: [picker('Harder?')] }
    expect(surfaceMessages(a, b, { answers: { q1: ['a'] } })).toContainEqual({ version: 'v0.9.1', updateDataModel: { surfaceId: 's1', path: '/answers/q1', value: [] } })
    expect(surfaceMessages(a, { ...a }, {})).toHaveLength(1)
  })
})

describe('keepLocalEdits', () => {
  it('lets a structural change in the new revision win over a leaf edit', () => {
    expect(keepLocalEdits({ a: { b: 1 } }, { a: 'x' })).toEqual({ a: { b: 1 } })
  })
})

describe('action text', () => {
  it('round-trips name and surface id through the persisted user line', () => {
    const text = a2uiActionText({ surface_id: 's1', name: 'submit_quiz', source_component_id: 'b', context: {} })
    expect(parseA2uiActionText(text)).toEqual({ name: 'submit_quiz', surfaceId: 's1', sourceComponentId: 'b', context: {} })
  })

  it('ignores ordinary and malformed text', () => {
    expect(parseA2uiActionText('hello')).toBeNull()
    expect(parseA2uiActionText('[a2ui_action] {nope')).toBeNull()
  })
})

describe('surfacesByTurn', () => {
  const rev = (revision: number, extra: Record<string, string> = {}) => ({ revision, mime_type: 'application/json', size: 1, ...extra })
  const turns = [{ id: 't1', created_at: '2026-09-27T10:00:00Z' }, { id: 't2', created_at: '2026-09-27T10:05:00Z' }]
  const list = (...revisions: ReturnType<typeof rev>[]) => ({
    data: [
      { name: 'a2ui_surface:s1', kind: 'a2ui_surface', revisions },
      { name: 'quiz_key:s1', kind: 'quiz_key', revisions: [rev(1, { turn_id: 't1' })] },
    ],
  }) as ArtifactList

  it('uses a revision turn_id that names a chat turn, at the latest revision', () => {
    expect(surfacesByTurn(list(rev(2, { turn_id: 't2' }), rev(1, { turn_id: 't1' })), turns)).toEqual({ t1: [{ name: 'a2ui_surface:s1', revision: 2 }] })
  })

  it('falls back to the live-turn pin when turn_id is not a chat turn id', () => {
    expect(surfacesByTurn(list(rev(1, { turn_id: 'adk-invocation-7' })), [...turns, { id: 'live' }], { 'a2ui_surface:s1': 'live' }))
      .toEqual({ live: [{ name: 'a2ui_surface:s1', revision: 1 }] })
  })

  it('falls back to the last turn started before the first revision', () => {
    expect(surfacesByTurn(list(rev(1, { turn_id: '', created_at: '2026-09-27T10:03:00Z' })), turns)).toEqual({ t1: [{ name: 'a2ui_surface:s1', revision: 1 }] })
    expect(surfacesByTurn(list(rev(1)), turns)).toEqual({})
  })
})

describe('restoredPicks', () => {
  const surface: SurfaceContent = {
    surface_id: 's1',
    components: [{ id: 'submit', component: 'Button', child: 'l', action: { event: { name: 'submit_quiz', context: { answers: { path: '/answers' } } } } }],
    data_model: { answers: { q1: [], q2: [] } },
  }
  const turn = (answers: unknown, surface_id = 's1') => a2uiActionText({ surface_id, name: 'submit_quiz', source_component_id: 'submit', context: { answers } })

  it('restores the latest action turn context onto blank bound paths', () => {
    const texts = ['hi', turn({ q1: ['a'] }), turn({ q1: ['c'], q2: ['b'] }), turn({ q1: ['x'] }, 'other')]
    expect(restoredPicks(surface, texts)).toEqual([{ path: '/answers', value: { q1: ['c'], q2: ['b'] } }])
  })

  it('leaves a stored data model with values alone, and does nothing without an action turn', () => {
    expect(restoredPicks({ ...surface, data_model: { answers: { q1: ['b'] } } }, [turn({ q1: ['c'] })])).toEqual([])
    expect(restoredPicks(surface, ['hi'])).toEqual([])
  })
})
