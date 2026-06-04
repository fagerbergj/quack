export interface ChatSummary {
  id: string
  title: string | null
  system_prompt: string | null
  created_at: string
  updated_at: string
}

export interface ChatDetail extends ChatSummary {
  messages: ChatMessage[]
}

export interface ChatMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
  created_at: string
  external_id?: string | null
}

export interface PaginatedChats {
  data: ChatSummary[]
  next_page_token?: string | null
}

export const api = {
  listChats: async (params?: { page_size?: number; before_id?: string }): Promise<PaginatedChats> => {
    const qs = new URLSearchParams()
    if (params?.page_size) qs.set('page_size', String(params.page_size))
    if (params?.before_id) qs.set('before_id', params.before_id)
    const res = await fetch(`/api/v1/chats?${qs}`)
    const json = await res.json()
    if (!res.ok) throw new Error(json.error ?? 'Failed')
    return json
  },

  createChat: async (opts?: { system_prompt?: string }): Promise<ChatSummary> => {
    const res = await fetch('/api/v1/chats', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ system_prompt: opts?.system_prompt }),
    })
    const json = await res.json()
    if (!res.ok) throw new Error(json.error ?? 'Failed')
    return json
  },

  getChat: async (chatId: string): Promise<ChatDetail> => {
    const res = await fetch(`/api/v1/chats/${chatId}`)
    const json = await res.json()
    if (!res.ok) throw new Error(json.error ?? 'Failed')
    return json
  },

  patchChat: async (chatId: string, patch: { title?: string | null; system_prompt?: string | null }) => {
    const res = await fetch(`/api/v1/chats/${chatId}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(patch),
    })
    if (!res.ok) {
      const json = await res.json()
      throw new Error(json.error ?? 'Failed')
    }
  },

  deleteChat: async (chatId: string) => {
    const res = await fetch(`/api/v1/chats/${chatId}`, { method: 'DELETE' })
    if (!res.ok) {
      const json = await res.json()
      throw new Error(json.error ?? 'Failed')
    }
  },

  sendMessage: (chatId: string, content: string, signal?: AbortSignal): Promise<Response> =>
    fetch(`/api/v1/chats/${chatId}/messages`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content }),
      signal,
    }),

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
