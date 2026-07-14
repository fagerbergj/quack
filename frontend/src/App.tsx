import Chat from './pages/Chat'

export default function App() {
  // The Chat page owns the full-screen layout and its own chat-list sidebar.
  // Routing is a single URL param (/chat/:chatId) handled in src/router.ts.
  return <Chat />
}
