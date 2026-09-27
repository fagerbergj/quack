import { describe, it, expect } from 'vitest'
import { a2uiActionText, keepLocalEdits, parseA2uiActionText, surfaceEventKey, surfaceMessages, surfacesByTurn, QUACK_CATALOG_ID, type SurfaceContent } from './a2ui'
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

describe('keepLocalEdits', () => {
  it('lets a structural change in the new revision win over a leaf edit', () => {
    expect(keepLocalEdits({ a: { b: 1 } }, { a: 'x' })).toEqual({ a: { b: 1 } })
  })
})

describe('action text', () => {
  it('round-trips name and surface id through the persisted user line', () => {
    const text = a2uiActionText({ surface_id: 's1', name: 'submit_quiz', source_component_id: 'b', context: {} })
    expect(parseA2uiActionText(text)).toEqual({ name: 'submit_quiz', surfaceId: 's1' })
  })

  it('ignores ordinary and malformed text', () => {
    expect(parseA2uiActionText('hello')).toBeNull()
    expect(parseA2uiActionText('[a2ui_action] {nope')).toBeNull()
  })
})

describe('surfacesByTurn', () => {
  it('places each surface at its first revision turn with its latest revision', () => {
    const list = {
      data: [
        { name: 'a2ui_surface:s1', kind: 'a2ui_surface', revisions: [
          { revision: 2, turn_id: 't2', mime_type: 'application/json', size: 1 },
          { revision: 1, turn_id: 't1', mime_type: 'application/json', size: 1 },
        ] },
        { name: 'quiz_key:s1', kind: 'quiz_key', revisions: [{ revision: 1, turn_id: 't1', mime_type: 'application/json', size: 1 }] },
      ],
    } as ArtifactList
    expect(surfacesByTurn(list)).toEqual({ t1: [{ name: 'a2ui_surface:s1', revision: 2 }] })
  })
})

describe('surfaceEventKey', () => {
  it('keys only a2ui_surface revisions', () => {
    expect(surfaceEventKey({ revision: { id: 'a2ui_surface:s1', revision: 2, kind: 'a2ui_surface' } })).toBe('a2ui_surface:s1@2')
    expect(surfaceEventKey({ revision: { id: 'text:x', revision: 2, kind: 'text' } })).toBe('')
    expect(surfaceEventKey(undefined)).toBe('')
  })
})
