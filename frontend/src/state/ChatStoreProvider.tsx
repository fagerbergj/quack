import { createContext, useContext, useMemo, useSyncExternalStore, type ReactNode } from 'react'
import { ChatStore, EMPTY_TURN, type ChatTurnState } from './chatStore'

const ChatStoreContext = createContext<ChatStore | null>(null)

/**
 * ChatStoreProvider mounts a single ChatStore above the router so the
 * stream state survives route changes. Without this, navigating away from
 * the chat page unmounts the component and drops the in-flight stream.
 */
export function ChatStoreProvider({ children }: { children: ReactNode }) {
  const store = useMemo(() => new ChatStore(), [])
  return <ChatStoreContext.Provider value={store}>{children}</ChatStoreContext.Provider>
}

export function useChatStore(): ChatStore {
  const s = useContext(ChatStoreContext)
  if (!s) throw new Error('useChatStore must be used inside <ChatStoreProvider>')
  return s
}

const noopSubscribe = () => () => {}
const getEmptyTurn = () => EMPTY_TURN

/**
 * useChatTurn subscribes a component to a specific chat's stream state.
 * Re-renders only when that chat's state changes — switching chats or
 * sending a token on another chat does not invalidate this hook.
 *
 * Pass chatId=null for the empty-state case (no active chat); the hook
 * returns the empty default without subscribing.
 */
export function useChatTurn(chatId: string | null): ChatTurnState {
  const store = useChatStore()
  const subscribe = useMemo(
    () => (chatId ? (listener: () => void) => store.subscribe(chatId, listener) : noopSubscribe),
    [store, chatId],
  )
  const getSnapshot = useMemo(
    () => (chatId ? () => store.get(chatId) : getEmptyTurn),
    [store, chatId],
  )
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}
