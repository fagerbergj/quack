// Quack REST client: types and SDK are generated from ../../openapi.yaml (`npm run generate`); this module unwraps
// results and throws on error. Streaming lives in the chat store.
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
  listPlugins as sdkListPlugins,
  createPlugin as sdkCreatePlugin,
  deletePlugin as sdkDeletePlugin,
  listPluginUpdates as sdkListPluginUpdates,
  updatePlugin as sdkUpdatePlugin,
  updateAllPlugins as sdkUpdateAllPlugins,
  reloadPlugins as sdkReloadPlugins,
} from './generated'

export type { ChatSummary, ChatDetail, ChatList, Turn, Memory, MemoryList, ExtensionInfo, ClientConfig, ArtifactSummary, ArtifactRevisionInfo, NodeMemory, NodeMemoryList, MemoryStats, MemoryWeekStats, MemoryScopeStats, Plugin, PluginUpdate, PluginReloadReport } from './generated'

import type { ChatSummary, ChatDetail, ChatList, Turn, MemoryList, ExtensionInfo, ClientConfig, ArtifactList, ArtifactRevisionList, NodeMemoryList, MemoryStats, Plugin, PluginList, PluginUpdateList, PluginDeleted, PluginReloadReport, VoteMemoryBody, ListMemoriesData } from './generated'

// "none" clears the caller's own prior vote.
export type VoteDirection = VoteMemoryBody['vote']
type MemoryListQuery = NonNullable<ListMemoriesData['query']>
export type MemoryListSort = NonNullable<MemoryListQuery['sort']>

type Result<T> = { data?: T; error?: unknown; response?: Response }

// The thrown error keeps the HTTP status and, for a plugin 422, the reload report beside the message.
function unwrap<T>(r: Result<T>): T {
  if (!r.response || !r.response.ok || r.error !== undefined) {
    const msg =
      r.error && typeof r.error === 'object' && 'error' in r.error
        ? String((r.error as { error: unknown }).error)
        : `Request failed (${r.response ? r.response.status : 'no response'})`
    const reload = r.error && typeof r.error === 'object' && 'reload' in r.error ? r.error.reload : undefined
    throw Object.assign(new Error(msg), { reload, status: r.response?.status })
  }
  return r.data as T
}

// For bodiless deletes: only the status matters.
function expectOk(r: Result<unknown>, what: string): void {
  if (!r.response || !r.response.ok) {
    throw new Error(`${what} failed (${r.response ? r.response.status : 'no response'})`)
  }
}

export const api = {
  // page_token is opaque: pass back exactly what next_page_token gave. status is multi-select (default ['active']);
  // a token is only valid for the set it was issued for, so switching restarts the walk. An empty array is a 400.
  listChats: async (opts?: { limit?: number; page_token?: string; status?: Array<'active' | 'archived'> }): Promise<ChatList> =>
    unwrap(await sdkListChats({ query: opts })),

  createChat: async (opts?: { system_prompt?: string }): Promise<ChatSummary> =>
    unwrap(await sdkCreateChat({ body: { system_prompt: opts?.system_prompt } })),

  getChat: async (chatId: string): Promise<ChatDetail> =>
    unwrap(await sdkGetChat({ path: { chat_id: chatId } })),

  deleteChat: async (chatId: string): Promise<void> =>
    expectOk(await sdkDeleteChat({ path: { chat_id: chatId } }), 'Delete'),

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
  listMemories: async (params: MemoryListQuery): Promise<MemoryList> => unwrap(await sdkListMemories({ query: params })),

  forgetMemory: async (id: string): Promise<void> =>
    expectOk(await sdkDeleteMemory({ path: { memory_id: id } }), 'Forget'),

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

  // Raw unified diff text; the panel maps a thrown 413 (>256KB) / 415 (binary) itself.
  // The 413 body embeds the artifact's id; never render it outside Details.
  diffArtifactRevisions: async (chatId: string, artifactName: string, from: number, to: number): Promise<string> =>
    unwrap(await sdkDiffArtifactRevisions({ path: { chat_id: chatId, artifact_name: artifactName }, query: { from, to } })),

  // Plain fetch, not the generated client: the response is
  // application/octet-stream (any mime) and the panel only wants text - a Blob round-trip would just get .text()'d.
  getArtifactText: async (chatId: string, artifactName: string, revision?: number): Promise<string> => {
    const res = await fetch(artifactUrl(chatId, artifactName, revision))
    if (!res.ok) throw new Error(`Fetch artifact failed (${res.status})`)
    return res.text()
  },

  listPlugins: async (): Promise<PluginList> => unwrap(await sdkListPlugins()),

  // 400/409 bodies carry the message the page shows inline (bad entry syntax
  // / name collision) - unwrap already surfaces error.error verbatim.
  createPlugin: async (entry: string): Promise<Plugin> =>
    unwrap(await sdkCreatePlugin({ body: { entry } })),

  deletePlugin: async (name: string): Promise<PluginDeleted> =>
    unwrap(await sdkDeletePlugin({ path: { name } })),

  listPluginUpdates: async (): Promise<PluginUpdateList> => unwrap(await sdkListPluginUpdates()),

  updatePlugin: async (name: string): Promise<Plugin> =>
    unwrap(await sdkUpdatePlugin({ path: { name } })),

  updateAllPlugins: async (): Promise<PluginList> => unwrap(await sdkUpdateAllPlugins()),

  // A 422 body is the report itself (nothing swapped), so it resolves as aborted rather than throwing.
  reloadPlugins: async (): Promise<{ reload: PluginReloadReport; aborted: boolean }> => {
    const r = await sdkReloadPlugins()
    if (r.response?.status === 422 && r.error) return { reload: r.error, aborted: true }
    return { reload: unwrap(r), aborted: false }
  },
}

// The REST path of one artifact revision (base '/'), for the panel's
// Details disclosure link.
export function artifactUrl(chatId: string, artifactName: string, revision?: number): string {
  return `/api/v1/chats/${encodeURIComponent(chatId)}/artifacts/${encodeURIComponent(artifactName)}${revision != null ? `?revision=${revision}` : ''}`
}

// Coalesces concurrent calls into one request - every finished node's card
// fires this on the same render pass, else an N-node DAG issues N identical GETs.
const inFlightArtifactLists = new Map<string, Promise<ArtifactList>>()
export function listChatArtifactsShared(chatId: string): Promise<ArtifactList> {
  let p = inFlightArtifactLists.get(chatId)
  if (!p) {
    p = api.listChatArtifacts(chatId).finally(() => inFlightArtifactLists.delete(chatId))
    inFlightArtifactLists.set(chatId, p)
  }
  return p
}
