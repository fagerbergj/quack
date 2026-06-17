import { BrowserRouter, Routes, Route } from 'react-router-dom'
import Chat from './pages/Chat'

export default function App() {
  // The Chat page owns the full-screen layout and its own chat-list sidebar.
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<Chat />} />
        <Route path="/chat/:chatId?" element={<Chat />} />
      </Routes>
    </BrowserRouter>
  )
}
