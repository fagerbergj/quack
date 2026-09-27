import type { A2uiMessage } from '@a2ui/web_core/v0_9'
import type { ArtifactList, SendMessageBody } from '../generated'

export const QUACK_CATALOG_ID = 'https://quack.local/a2ui/v0_9/catalog.json'
export const A2UI_SURFACE_KIND = 'a2ui_surface'
export const QUIZ_KEY_KIND = 'quiz_key'

// One `a2ui_surface` artifact revision (flat form; the model never writes envelopes).
export interface SurfaceContent {
  surface_id: string
  catalog_id?: string
  components: Array<{ id: string; component: string } & Record<string, unknown>>
  data_model?: Record<string, unknown>
}

export interface A2uiActionRequest {
  surface_id: string
  name: string
  source_component_id: string
  context: Record<string, unknown>
}

// Intersection, not a copy: still type-checks once the regenerated client declares a2ui_action itself.
export type SendMessageBodyWithAction = SendMessageBody & { a2ui_action?: A2uiActionRequest }

type Json = Record<string, unknown>
const isRecord = (v: unknown): v is Json => typeof v === 'object' && v !== null && !Array.isArray(v)

// The new revision's shape wins; the user's value survives wherever its path still exists.
export function keepLocalEdits(next: unknown, local: unknown): unknown {
  if (isRecord(next) && isRecord(local)) {
    return Object.fromEntries(Object.entries(next).map(([k, v]) => [k, k in local ? keepLocalEdits(v, local[k]) : v]))
  }
  if (isRecord(next) || isRecord(local)) return next
  return local
}

// prev undefined = first render. An unchanged data_model is not re-sent, so local edits stay untouched.
export function surfaceMessages(prev: SurfaceContent | undefined, next: SurfaceContent, localData: unknown): A2uiMessage[] {
  const surfaceId = next.surface_id
  const version = 'v0.9.1' as const
  const msgs: A2uiMessage[] = []
  if (!prev) msgs.push({ version, createSurface: { surfaceId, catalogId: next.catalog_id || QUACK_CATALOG_ID } })
  msgs.push({ version, updateComponents: { surfaceId, components: next.components } })
  const data = next.data_model ?? {}
  if (!prev) {
    msgs.push({ version, updateDataModel: { surfaceId, path: '/', value: data } })
  } else if (JSON.stringify(prev.data_model ?? {}) !== JSON.stringify(data)) {
    msgs.push({ version, updateDataModel: { surfaceId, path: '/', value: keepLocalEdits(data, localData) } })
  }
  return msgs
}

const ACTION_PREFIX = '[a2ui_action] '

// The user text the backend persists for an action turn; mirrored for the optimistic live bubble.
export function a2uiActionText(action: A2uiActionRequest): string {
  return ACTION_PREFIX + JSON.stringify(action)
}

export function parseA2uiActionText(text: string): { name: string; surfaceId: string } | null {
  if (!text.startsWith(ACTION_PREFIX)) return null
  try {
    const a = JSON.parse(text.slice(ACTION_PREFIX.length)) as Partial<A2uiActionRequest> | null
    if (typeof a?.name === 'string' && typeof a.surface_id === 'string') return { name: a.name, surfaceId: a.surface_id }
  } catch { /* not an action turn */ }
  return null
}

// Changes once per new a2ui_surface revision announced on the stream; '' for every other artifact event.
export function surfaceEventKey(events: { revision?: { id: string; revision: number; kind: string } } | undefined): string {
  const rev = events?.revision
  return rev?.kind === A2UI_SURFACE_KIND ? `${rev.id}@${rev.revision}` : ''
}

export interface SurfaceRef { name: string; revision: number }

// A surface renders once, at the turn that created it, always at its latest
// revision - later render_ui calls update it in place rather than re-posting it.
export function surfacesByTurn(artifacts: ArtifactList): Record<string, SurfaceRef[]> {
  const byTurn: Record<string, SurfaceRef[]> = {}
  for (const a of artifacts.data) {
    if (a.kind !== A2UI_SURFACE_KIND || a.revisions.length === 0) continue
    const sorted = [...a.revisions].sort((x, y) => x.revision - y.revision)
    const turnId = sorted[0].turn_id
    if (!turnId) continue
    ;(byTurn[turnId] ??= []).push({ name: a.name, revision: sorted[sorted.length - 1].revision })
  }
  return byTurn
}
