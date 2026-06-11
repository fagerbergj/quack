// Quack REST client. Types and the request SDK are generated from the single
// source of truth, ../../openapi.yaml (see `npm run generate`); this module is a
// thin ergonomic wrapper that unwraps the generated result objects and throws on
// error. The streaming responses endpoint is handled directly by the chat store.
import {
  listChats as sdkListChats,
  createChat as sdkCreateChat,
  getChat as sdkGetChat,
  deleteChat as sdkDeleteChat,
  getResponse as sdkGetResponse,
} from './generated'

export type {
  ChatSummary,
  ChatDetail,
  ChatList,
  Turn,
  TurnInput,
  OutputItem,
  MessageOutputItem,
  DagOutputItem,
  ContentPart,
  OutputTextPart,
  ReasoningPart,
  DagNodeDef,
  DagEdge,
  DagNodeState,
  ItemStatus,
} from './generated'

import type { ChatSummary, ChatDetail, ChatList, Turn } from './generated'

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
  listChats: async (): Promise<ChatList> => unwrap(await sdkListChats()),

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

  getResponse: async (chatId: string, responseId: string): Promise<Turn | null> => {
    const r = await sdkGetResponse({ path: { chat_id: chatId, response_id: responseId } })
    if (r.response?.status === 404) return null
    return unwrap(r)
  },
}
