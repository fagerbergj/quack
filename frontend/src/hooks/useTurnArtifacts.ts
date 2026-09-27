import { useEffect, useMemo, useState } from 'react'
import { api } from '../api'
import type { ArtifactList } from '../generated'
import type { ChatState } from '../state/chatStore'
import { imageAttachmentsByTurn } from '../lib/turnAttachments'
import type { AttachmentPreview } from '../components/AttachmentUI'
import { surfacesByTurn } from '../lib/a2ui'

const EMPTY: ArtifactList = { data: [] }
const NO_IMAGES: Record<string, AttachmentPreview[]> = {}

// #1138: per-turn image thumbnails and A2UI surfaces from the chat's artifacts. Refetched per archived
// turn (an attachment turn archives when the NEXT one is sent) and per a2ui_surface revision event.
export function useTurnArtifacts(chatId: string | null, state: ChatState) {
  const [artifacts, setArtifacts] = useState<{ chatId: string | null; list: ArtifactList; images: Record<string, AttachmentPreview[]> }>({ chatId: null, list: EMPTY, images: {} })
  useEffect(() => {
    if (!chatId) return
    let cancelled = false
    // Mapped inside the promise: a malformed list is dropped here, never thrown during render.
    api.listChatArtifacts(chatId).then(list => {
      if (!cancelled) setArtifacts({ chatId, list, images: imageAttachmentsByTurn(chatId, list) })
    }).catch(() => {})
    return () => { cancelled = true }
  }, [chatId, state.turns.length, state.surfaceSeq])

  const current = artifacts.chatId === chatId
  const list = current ? artifacts.list : EMPTY
  const turnImages = current ? artifacts.images : NO_IMAGES
  const live = state.live
  const turnSurfaces = useMemo(
    () => surfacesByTurn(list, live?.id ? [...state.turns, { id: live.id, created_at: live.createdAt }] : state.turns, state.surfacePins),
    [list, state.turns, live?.id, live?.createdAt, state.surfacePins],
  )
  return { turnImages, turnSurfaces }
}
