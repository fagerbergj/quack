import { useEffect } from 'react'
import Chat from './pages/Chat'
import GitHubSessions from './pages/GitHubSessions'
import { navigate, useView } from './router'

function NavButton({ label, active, onClick }: { label: string; active: boolean; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      className={`text-sm px-3 py-1 rounded-md transition-colors ${
        active
          ? 'bg-blue-600 text-white'
          : 'text-gray-600 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700'
      }`}
    >
      {label}
    </button>
  )
}

export default function App() {
  const view = useView()

  // Theme init must run here, not in Chat, so routes other than "/" (e.g. /github)
  // also respect the stored/prefers-color-scheme theme on first load.
  useEffect(() => {
    const stored = localStorage.getItem('theme')
    if (stored === 'dark' || (!stored && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
      document.documentElement.classList.add('dark')
    }
  }, [])

  return (
    <div className="h-screen flex flex-col">
      <div className="flex-shrink-0 flex items-center gap-1 px-3 py-1.5 border-b border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800">
        <NavButton label="Chat" active={view === 'chat'} onClick={() => navigate('/')} />
        <NavButton label="GitHub" active={view === 'github'} onClick={() => navigate('/github')} />
      </div>
      <div className="flex-1 min-h-0">
        {/* The Chat page owns the full-screen layout and its own chat-list
            sidebar; Routing is a single URL param (/chat/:chatId) handled in
            src/router.ts. */}
        {view === 'github' ? <GitHubSessions /> : <Chat />}
      </div>
    </div>
  )
}
