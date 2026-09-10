// Quack REST client. Types and the request SDK are generated from
// ../../openapi.yaml (`npm run generate`); this module only unwraps the
// generated results and throws on error. Streaming is handled by the chat store.
import {
  listChats as sdkListChats,
  createChat as sdkCreateChat,
  getChat as sdkGetChat,
  deleteChat as sdkDeleteChat,
  updateChat as sdkUpdateChat,
  getResponse as sdkGetResponse,
  listMemories as sdkListMemories,
  deleteMemory as sdkDeleteMemory,
  voteMemory as sdkVoteMemory,
  listNodeMemories as sdkListNodeMemories,
  listExtensions as sdkListExtensions,
  getMemoryStats as sdkGetMemoryStats,
  getConfig as sdkGetConfig,
  listChatArtifacts as sdkListChatArtifacts,
  listArtifactRevisions as sdkListArtifactRevisions,
  diffArtifactRevisions as sdkDiffArtifactRevisions,
} from './generated'

export type { ChatSummary, ChatDetail, ChatList, Turn, Memory, MemoryList, ExtensionInfo, ClientConfig, ArtifactSummary, ArtifactRevisionInfo, NodeMemory, NodeMemoryList, MemoryStats, MemoryWeekStats, MemoryScopeStats } from './generated'

import type { ChatSummary, ChatDetail, ChatList, Turn, MemoryList, ExtensionInfo, ClientConfig, ArtifactList, ArtifactRevisionList, NodeMemoryList, MemoryStats } from './generated'

// VoteDirection is the UI-facing shape of a manual vote - "none" clears the
// caller's own prior vote (the Reddit-style toggle-off), matching the
// backend's VoteMemoryBody vote enum.
export type VoteDirection = 'up' | 'down' | 'none'

// MemoryListSort mirrors openapi.yaml's listMemories `sort` enum (#1266) -
// server-side ordering that spans every page, not a client re-sort of one.
export type MemoryListSort = 'newest' | 'oldest' | 'score' | 'upvotes' | 'downvotes' | 'recalls' | 'last_recalled'

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
  // page_token is opaque - pass back exactly what next_page_token gave.
  // status is a multi-select (default ['active']); a token is only valid against the exact set it was issued for, so switching restarts the walk. An explicitly empty array is a 400.
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
    tier?: 'unverified' | 'verified'
    sort?: MemoryListSort
  }): Promise<MemoryList> => unwrap(await sdkListMemories({ query: params })),

  forgetMemory: async (id: string): Promise<void> => {
    const r = await sdkDeleteMemory({ path: { memory_id: id } })
    if (!r.response || !r.response.ok) {
      throw new Error(`Forget failed (${r.response ? r.response.status : 'no response'})`)
    }
  },

  voteMemory: async (id: string, vote: VoteDirection, reason?: string) =>
    unwrap(await sdkVoteMemory({ path: { memory_id: id }, body: { vote, reason } })),

  listNodeMemories: async (chatId: string, nodeId: string): Promise<NodeMemoryList> =>
    unwrap(await sdkListNodeMemories({ path: { chat_id: chatId, node_id: nodeId } })),

  listExtensions: async (): Promise<ExtensionInfo[]> => unwrap(await sdkListExtensions()),

  // weeks defaults server-side to 12; the header only shows the last 4 but
  // asks for that default so a future "see more" needs no new request shape.
  getMemoryStats: async (weeks?: number): Promise<MemoryStats> =>
    unwrap(await sdkGetMemoryStats({ query: { weeks } })),

  getConfig: async (): Promise<ClientConfig> => unwrap(await sdkGetConfig()),

  listChatArtifacts: async (chatId: string): Promise<ArtifactList> =>
    unwrap(await sdkListChatArtifacts({ path: { chat_id: chatId } })),

  listArtifactRevisions: async (chatId: string, artifactName: string): Promise<ArtifactRevisionList> =>
    unwrap(await sdkListArtifactRevisions({ path: { chat_id: chatId, artifact_name: artifactName } })),

  // Raw unified diff text (text/plain, not JSON); unlike the other unwrap()
  // callers this keeps the HTTP status on the thrown error - the panel maps
  // 413 (>256KB) / 415 (binary) itself. The 413 body embeds the artifact's id; never render it outside Details (#1178).
  diffArtifactRevisions: async (chatId: string, artifactName: string, from: number, to: number): Promise<string> => {
    const r = await sdkDiffArtifactRevisions({ path: { chat_id: chatId, artifact_name: artifactName }, query: { from, to } })
    if (!r.response || !r.response.ok || r.error !== undefined) {
      const msg =
        r.error && typeof r.error === 'object' && 'error' in r.error
          ? String((r.error as { error: unknown }).error)
          : `Request failed (${r.response ? r.response.status : 'no response'})`
      const err = new Error(msg) as Error & { status?: number }
      err.status = r.response?.status
      throw err
    }
    return r.data as string
  },

  // Plain fetch, not the generated client: the response is
  // application/octet-stream (any mime) and the panel only wants text - a Blob round-trip would just get .text()'d.
  getArtifactText: async (chatId: string, artifactName: string, revision?: number): Promise<string> => {
    const res = await fetch(artifactUrl(chatId, artifactName, revision))
    if (!res.ok) throw new Error(`Fetch artifact failed (${res.status})`)
    return res.text()
  },
}

// The REST path of one artifact revision (base '/'), for the panel's
// Details disclosure link.
export function artifactUrl(chatId: string, artifactName: string, revision?: number): string {
  return `/api/v1/chats/${encodeURIComponent(chatId)}/artifacts/${encodeURIComponent(artifactName)}${revision != null ? `?revision=${revision}` : ''}`
}
