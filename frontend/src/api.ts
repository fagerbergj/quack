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
} from './generated'

export type { ChatSummary, ChatDetail, ChatList, Turn, Memory, MemoryList } from './generated'

import type { ChatSummary, ChatDetail, ChatList, Turn, MemoryList } from './generated'

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

  listMemories: async (params: { bucket?: string; q?: string; limit?: number; offset?: number }): Promise<MemoryList> =>
    unwrap(await sdkListMemories({ query: params })),

  forgetMemory: async (id: string): Promise<void> => {
    const r = await sdkDeleteMemory({ path: { memory_id: id } })
    if (!r.response || !r.response.ok) {
      throw new Error(`Forget failed (${r.response ? r.response.status : 'no response'})`)
    }
  },
}
