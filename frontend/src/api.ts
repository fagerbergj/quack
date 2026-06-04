/**
 * Thin wrapper around the generated SDK. Pages import from here for clean names.
 * All API calls use the generated functions so URLs/types stay in sync with openapi.yaml.
 * SSE streaming endpoints use raw fetch since they need manual stream handling.
 */
import {
  listChats as listChatsApiV1ChatsGet,
  createChat as createChatApiV1ChatsPost,
  getChat as getChatApiV1ChatsChatIdGet,
  patchChat as patchChatApiV1ChatsChatIdPatch,
  deleteChat as deleteChatApiV1ChatsChatIdDelete,
} from './generated'

export type {
  DocumentDetail,
  DocumentSummary,
  JobDetail,
  JobSummary,
  JobStatus,
  JobOptions,
  PaginatedJobs,
  PaginatedContexts,
  ContextEntry,
  PatchDocumentBody,
  CreateContextBody,
  UpdateContextBody,
  Artifact,
  Run,
  RunIoField,
  RunQuestion,
  RunSuggestions,
  PipelineDetail,
  StageSummary,
  StageDetail,
} from './generated'

// Chat types (generator returns unknown for these responses)
export interface ChatMessage {
  id: string
  external_id?: string | null
  role: 'user' | 'assistant'
  content: string
  created_at: string
}

export interface RagRetrieval {
  enabled?: boolean | null
  max_sources?: number | null
  minimum_score?: number | null
}

export interface ChatSummary {
  id: string
  title: string | null
  system_prompt: string | null
  rag_retrieval: RagRetrieval | null
  created_at: string
  updated_at: string
}

export interface ChatDetail extends ChatSummary {
  messages: ChatMessage[]
}

export interface PaginatedChats {
  data: ChatSummary[]
  next_page_token?: string | null
}

async function unwrap<T>(call: Promise<{ data?: T; error?: unknown }>): Promise<T> {
  const { data, error } = await call
  if (error) throw error
  return data as T
}

export const api = {
  // ── Chats ─────────────────────────────────────────────────────────────────
  listChats: (params?: { page_size?: number; before_id?: string }) =>
    unwrap(listChatsApiV1ChatsGet({ query: { page_size: params?.page_size, before_id: params?.before_id } })) as Promise<PaginatedChats>,

  createChat: (opts?: { system_prompt?: string; rag_retrieval?: RagRetrieval }) =>
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    unwrap(createChatApiV1ChatsPost({ body: { system_prompt: opts?.system_prompt, rag_retrieval: opts?.rag_retrieval } as any })) as Promise<ChatSummary>,

  getChat: (chatId: string) =>
    unwrap(getChatApiV1ChatsChatIdGet({ path: { chat_id: chatId } })) as Promise<ChatDetail>,

  patchChat: (chatId: string, patch: { title?: string | null; system_prompt?: string | null; rag_retrieval?: RagRetrieval | null }) =>
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    unwrap(patchChatApiV1ChatsChatIdPatch({ path: { chat_id: chatId }, body: patch as any })) as Promise<ChatSummary>,

  deleteChat: (chatId: string) =>
    unwrap(deleteChatApiV1ChatsChatIdDelete({ path: { chat_id: chatId } })),

  // ── Chat message streaming (SSE — manual fetch needed) ────────────────────
  sendMessage: (chatId: string, content: string, signal?: AbortSignal): Promise<Response> =>
    fetch(`/api/v1/chats/${chatId}/messages`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content }),
      signal,
    }),

  // decideConfirmation submits the user's approve/reject decision for a
  // pending tool-call confirmation. The response body is an SSE stream
  // (continuation on approve, single `done` event on reject).
  decideConfirmation: (
    chatId: string,
    callId: string,
    body: { confirmed: boolean; content?: string },
    signal?: AbortSignal,
  ): Promise<Response> =>
    fetch(`/api/v1/chats/${chatId}/confirmations/${callId}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal,
    }),
}
