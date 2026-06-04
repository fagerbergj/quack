// This file is auto-generated from openapi.yaml
// DO NOT EDIT

export interface CreateChatBody {
  system_prompt?: string | null
  rag_retrieval?: RagRetrieval | null
}

export interface UpdateChatBody {
  title?: string | null
  system_prompt?: string | null
  rag_retrieval?: RagRetrieval | null
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

export interface ChatMessage {
  id: string
  external_id?: string | null
  role: 'user' | 'assistant'
  content: string
  created_at: string
}

export interface PaginatedChats {
  data: ChatSummary[]
  next_page_token?: string | null
}

export interface CreateChatRequest {
  system_prompt?: string | null
  rag_retrieval?: RagRetrieval | null
}

export interface PatchChatRequest {
  title?: string | null
  system_prompt?: string | null
  rag_retrieval?: RagRetrieval | null
}

export interface SendMessageRequest {
  content: string
}

export interface ConfirmRequest {
  confirmed: boolean
  content?: string
}
