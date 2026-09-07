import { createContext, useContext, useMemo, useSyncExternalStore, type ReactNode } from 'react'
import { ChatStore, EMPTY_STATE, type ChatState } from './chatStore'

const ChatStoreContext = createContext<ChatStore | null>(null)

// `store` is test-only: lets a test inject a ChatStore it can drive directly
// (dispatch a fake SSE event) instead of only reading through the provider.
export function ChatStoreProvider({ children, store: injected }: { children: ReactNode; store?: ChatStore }) {
  const own = useMemo(() => new ChatStore(), [])
  return <ChatStoreContext.Provider value={injected ?? own}>{children}</ChatStoreContext.Provider>
}

export function useChatStore(): ChatStore {
  const s = useContext(ChatStoreContext)
  if (!s) throw new Error('useChatStore must be used inside <ChatStoreProvider>')
  return s
}

const noopSubscribe = () => () => {}
const getEmptyState = () => EMPTY_STATE

export function useChatState(chatId: string | null): ChatState {
  const store = useChatStore()
  const subscribe = useMemo(
    () => (chatId ? (listener: () => void) => store.subscribe(chatId, listener) : noopSubscribe),
    [store, chatId],
  )
  const getSnapshot = useMemo(
    () => (chatId ? () => store.get(chatId) : getEmptyState),
    [store, chatId],
  )
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}
