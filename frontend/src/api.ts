// Quack REST client. Types and the request SDK are generated from the single
// source of truth, ../../openapi.yaml (see `npm run generate`); this module is a
// thin ergonomic wrapper that unwraps the generated result objects and throws on
// error. The streaming responses endpoint is handled directly by the chat store.
import {
  listChats as sdkListChats,
  createChat as sdkCreateChat,
  getChat as sdkGetChat,
  deleteChat as sdkDeleteChat,
  updateChat as sdkUpdateChat,
  getResponse as sdkGetResponse,
  listMemories as sdkListMemories,
  deleteMemory as sdkDeleteMemory,
  listExtensions as sdkListExtensions,
  getConfig as sdkGetConfig,
  listChatArtifacts as sdkListChatArtifacts,
  listArtifactRevisions as sdkListArtifactRevisions,
  diffArtifactRevisions as sdkDiffArtifactRevisions,
} from './generated'

export type { ChatSummary, ChatDetail, ChatList, Turn, Memory, MemoryList, ExtensionInfo, ClientConfig, ArtifactSummary, ArtifactRevisionInfo } from './generated'

import type { ChatSummary, ChatDetail, ChatList, Turn, MemoryList, ExtensionInfo, ClientConfig, ArtifactList, ArtifactRevisionList } from './generated'

type Result<T> = { data?: T; error?: unknown; response?: Response }

function unwrap<T>(r: Result<T>): T {
  if (!r.response || !r.response.ok || r.error !== undefined) {
    const msg =
      r.error && typeof r.error === 'object' && 'error' in r.error
        ? String((r.error as { error: unknown }).error)
        : `Request failed (${r.response ? r.response.status : 'no response'})`
    throw new Error(msg)
  }
  return r.data as T
}

export const api = {
  // page_token is opaque - pass back exactly what a previous response's
  // next_page_token gave, never parsed or constructed here. status is a
  // multi-select (default ['active']); order doesn't matter, but a token is
  // only valid against the exact status set it was issued for, so switching
  // it starts a fresh page walk. An explicitly empty array is a 400.
  listChats: async (opts?: { limit?: number; page_token?: string; status?: Array<'active' | 'archived'> }): Promise<ChatList> =>
    unwrap(await sdkListChats({ query: opts })),

  createChat: async (opts?: { system_prompt?: string }): Promise<ChatSummary> =>
    unwrap(await sdkCreateChat({ body: { system_prompt: opts?.system_prompt } })),

  getChat: async (chatId: string): Promise<ChatDetail> =>
    unwrap(await sdkGetChat({ path: { chat_id: chatId } })),

  deleteChat: async (chatId: string): Promise<void> => {
    const r = await sdkDeleteChat({ path: { chat_id: chatId } })
    if (!r.response || !r.response.ok) {
      throw new Error(`Delete failed (${r.response ? r.response.status : 'no response'})`)
    }
  },

  renameChat: async (chatId: string, title: string): Promise<ChatSummary> =>
    unwrap(await sdkUpdateChat({ path: { chat_id: chatId }, body: { title } })),

  archiveChat: async (chatId: string, archived: boolean): Promise<ChatSummary> =>
    unwrap(await sdkUpdateChat({ path: { chat_id: chatId }, body: { archived } })),

  getResponse: async (chatId: string, responseId: string): Promise<Turn | null> => {
    const r = await sdkGetResponse({ path: { chat_id: chatId, response_id: responseId } })
    if (r.response?.status === 404) return null
    return unwrap(r)
  },

  // page_token is opaque, same contract as listChats' - pass back exactly what
  // a previous response's next_page_token gave, never parsed or constructed here.
  listMemories: async (params: {
    bucket?: string
    q?: string
    limit?: number
    page_token?: string
    include_invalidated?: boolean
  }): Promise<MemoryList> => unwrap(await sdkListMemories({ query: params })),

  forgetMemory: async (id: string): Promise<void> => {
    const r = await sdkDeleteMemory({ path: { memory_id: id } })
    if (!r.response || !r.response.ok) {
      throw new Error(`Forget failed (${r.response ? r.response.status : 'no response'})`)
    }
  },

  listExtensions: async (): Promise<ExtensionInfo[]> => unwrap(await sdkListExtensions()),

  getConfig: async (): Promise<ClientConfig> => unwrap(await sdkGetConfig()),

  listChatArtifacts: async (chatId: string): Promise<ArtifactList> =>
    unwrap(await sdkListChatArtifacts({ path: { chat_id: chatId } })),

  listArtifactRevisions: async (chatId: string, artifactName: string): Promise<ArtifactRevisionList> =>
    unwrap(await sdkListArtifactRevisions({ path: { chat_id: chatId, artifact_name: artifactName } })),

  // Returns the raw unified diff text (endpoint answers text/plain, not JSON).
  diffArtifactRevisions: async (chatId: string, artifactName: string, from: number, to: number): Promise<string> =>
    unwrap(await sdkDiffArtifactRevisions({ path: { chat_id: chatId, artifact_name: artifactName }, query: { from, to } })),

  // Plain fetch, not the generated client: getChatArtifact's response is
  // application/octet-stream (any mime), and the panel only ever wants it as
  // text (markdown/JSON revisions) - a Blob round-trip would just get
  // .text()'d right back.
  getArtifactText: async (chatId: string, artifactName: string, revision?: number): Promise<string> => {
    const q = revision != null ? `?revision=${revision}` : ''
    const res = await fetch(`/api/v1/chats/${encodeURIComponent(chatId)}/artifacts/${encodeURIComponent(artifactName)}${q}`)
    if (!res.ok) throw new Error(`Fetch artifact failed (${res.status})`)
    return res.text()
  },
}
