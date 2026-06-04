import { useState } from 'react'
import { BrowserRouter, Routes, Route } from 'react-router-dom'
import Sidebar from './components/Sidebar'
import Chat from './pages/Chat'

export default function App() {
  const [systemPrompt, setSystemPrompt] = useState('')
  const [sidebarOpen, setSidebarOpen] = useState(false)

  return (
    <div className="flex min-h-screen bg-gray-950 text-gray-100">
      {sidebarOpen && <Sidebar open={sidebarOpen} onClose={() => setSidebarOpen(false)} />}
      {sidebarOpen && <div className="fixed inset-0 z-30 bg-black/50 md:hidden" onClick={() => setSidebarOpen(false)} />}
      <div className="flex-1 md:ml-64 bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-white min-h-screen">
        <button
          onClick={() => setSidebarOpen(true)}
          className="md:hidden fixed top-3 left-3 z-40 w-9 h-9 flex items-center justify-center rounded-lg bg-gray-800 text-gray-200 hover:bg-gray-700 transition-colors"
          aria-label="Open menu"
        >
          <span className="flex flex-col gap-1">
            <span className="block w-4 h-0.5 bg-current" />
            <span className="block w-4 h-0.5 bg-current" />
            <span className="block w-4 h-0.5 bg-current" />
          </span>
        </button>
        <BrowserRouter>
          <Routes>
            <Route path="/" element={<Chat systemPrompt={systemPrompt} />} />
            <Route path="/chat/:chatId?" element={<Chat systemPrompt={systemPrompt} />} />
          </Routes>
        </BrowserRouter>
      </div>
    </div>
  )
}
