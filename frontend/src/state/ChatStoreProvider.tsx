import { createContext, useContext, useMemo, useSyncExternalStore, type ReactNode } from 'react'
import { ChatStore, EMPTY_TURN, type ChatTurnState } from './chatStore'

const ChatStoreContext = createContext<ChatStore | null>(null)

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
