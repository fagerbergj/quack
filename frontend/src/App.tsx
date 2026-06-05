import { useState } from 'react'
import { BrowserRouter, Routes, Route } from 'react-router-dom'
import Chat from './pages/Chat'

export default function App() {
  // The Chat page owns the full-screen layout and its own chat-list sidebar.
  const [systemPrompt] = useState('')
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<Chat systemPrompt={systemPrompt} />} />
        <Route path="/chat/:chatId?" element={<Chat systemPrompt={systemPrompt} />} />
      </Routes>
    </BrowserRouter>
  )
}
