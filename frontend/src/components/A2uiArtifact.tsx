import { Component, Suspense, lazy, useCallback, useEffect, useState, type ReactNode } from 'react'
import { api } from '../api'
import { useChatState, useChatStore } from '../state/ChatStoreProvider'
import { surfacePersistKey, type SurfaceContent, type SurfaceRef } from '../lib/a2ui'
import type { A2UiAction } from '../generated'

// The renderer (web_core + zod) is its own chunk: most chats never show a surface.
const A2uiSurfaceView = lazy(() => import('./A2uiSurface'))

const loadFailed = <p className="text-xs text-amber-700 dark:text-amber-400">This interactive view couldn&apos;t load.</p>

// Model-authored content: a renderer crash or a failed chunk load stays inside this box.
class SurfaceBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false }
  static getDerivedStateFromError() { return { failed: true } }
  render() {
    if (this.state.failed) return loadFailed
    return this.props.children
  }
}

// A surface with its button actions sent as turns of this chat.
export function A2uiSurfaceBox({ chatId, content, revision, persistKey }: { chatId: string; content: SurfaceContent; revision?: number; persistKey?: string }) {
  const store = useChatStore()
  const state = useChatState(chatId)
  const busy = !!(state.submitting || state.live?.streaming)
  const onAction = useCallback((a: A2UiAction) => { void store.submitA2uiAction(chatId, a) }, [store, chatId])
  return (
    <SurfaceBoundary>
      <Suspense fallback={<p className="text-xs text-gray-500 dark:text-gray-400" role="status">Loading…</p>}>
        <A2uiSurfaceView content={content} revision={revision} onAction={onAction} persistKey={persistKey} busy={busy} />
      </Suspense>
    </SurfaceBoundary>
  )
}

// Inline transcript card for one a2ui_surface artifact, at the given (latest) revision.
function A2uiArtifact({ chatId, name, revision }: { chatId: string; name: string; revision: number }) {
  const [content, setContent] = useState<SurfaceContent | null>(null)
  const [error, setError] = useState(false)
  useEffect(() => {
    let cancelled = false
    api.getArtifactText(chatId, name, revision)
      .then(t => { if (!cancelled) setContent(JSON.parse(t) as SurfaceContent) })
      .catch(() => { if (!cancelled) setError(true) })
    return () => { cancelled = true }
  }, [chatId, name, revision])
  if (!content) return error ? loadFailed : null
  return <A2uiSurfaceBox chatId={chatId} content={content} revision={revision} persistKey={surfacePersistKey(chatId, name)} />
}

// The surfaces a turn created, under its response.
export function TurnSurfaces({ chatId, surfaces }: { chatId?: string; surfaces?: SurfaceRef[] }) {
  if (!chatId || !surfaces?.length) return null
  return (
    <div className="mt-3 space-y-3">
      {surfaces.map(s => <A2uiArtifact key={s.name} chatId={chatId} name={s.name} revision={s.revision} />)}
    </div>
  )
}
