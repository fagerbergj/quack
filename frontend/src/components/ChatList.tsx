import { useState } from 'react'
import type { ChatSummary } from '../api'
import { isGithubChat } from '../pages/GitHubSessions'

export type OriginFilter = 'all' | 'github' | 'direct'

// filterChatsByOrigin narrows by the same origin signal GitHubSessions.tsx
// uses (isGithubChat) — no separate signal to keep in sync.
export function filterChatsByOrigin(chats: ChatSummary[], filter: OriginFilter): ChatSummary[] {
  if (filter === 'all') return chats
  return chats.filter(c => (filter === 'github' ? isGithubChat(c) : !isGithubChat(c)))
}

// githubRef derives a short "Issue #N" / "PR #N" label from a github issue/PR
// URL (…/issues/N or …/pull/N), or null when the URL isn't one of those.
export function githubRef(url: string): { label: string; kind: 'issue' | 'pr' } | null {
  const m = url.match(/\/(issues|pull)\/(\d+)/)
  if (!m) return null
  return m[1] === 'pull' ? { label: `PR #${m[2]}`, kind: 'pr' } : { label: `Issue #${m[2]}`, kind: 'issue' }
}

function relativeDate(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime()
  const mins = Math.floor(diff / 60000)
  if (mins < 1) return 'just now'
  if (mins < 60) return `${mins}m ago`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h ago`
  const days = Math.floor(hrs / 24)
  if (days < 7) return `${days}d ago`
  return new Date(iso).toLocaleDateString()
}

export interface ChatListProps {
  chats: ChatSummary[]
  activeChatId: string | null
  open: boolean
  onSelect: (id: string) => void
  onNewChat: () => void
  onDelete: (id: string, e: React.MouseEvent) => void
  onCloseMobile: () => void
}

const ORIGIN_FILTERS: { value: OriginFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'direct', label: 'Direct' },
  { value: 'github', label: 'GitHub' },
]

export function ChatList({ chats, activeChatId, open, onSelect, onNewChat, onDelete, onCloseMobile }: ChatListProps) {
  const [query, setQuery] = useState('')
  const [originFilter, setOriginFilter] = useState<OriginFilter>('all')
  const q = query.trim().toLowerCase()
  const byOrigin = filterChatsByOrigin(chats, originFilter)
  const filtered = q ? byOrigin.filter(c => (c.title ?? '').toLowerCase().includes(q)) : byOrigin

  return (
    <div className={`
      fixed md:static inset-y-0 left-0 z-40
      h-screen w-[250px] flex-shrink-0 flex flex-col
      border-r border-gray-200 dark:border-gray-700
      bg-white dark:bg-gray-800
      transition-transform duration-200
      md:translate-x-0
      ${open ? 'translate-x-0' : '-translate-x-full md:translate-x-0'}
    `}>
      <div className="p-3 border-b border-gray-200 dark:border-gray-700 flex items-center gap-2">
        <button
          onClick={onNewChat}
          className="flex-1 text-sm px-3 py-2 rounded-lg bg-blue-600 text-white hover:bg-blue-700 transition-colors font-medium"
        >
          New Chat
        </button>
        <button
          onClick={onCloseMobile}
          className="md:hidden text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 p-1.5 rounded transition-colors"
          aria-label="Close chat list"
        >
          ✕
        </button>
      </div>
      <div className="p-2 border-b border-gray-200 dark:border-gray-700">
        <input
          type="search"
          value={query}
          onChange={e => setQuery(e.target.value)}
          placeholder="Search chats…"
          aria-label="Search chats"
          className="w-full rounded-lg border border-gray-300 dark:border-gray-600 px-3 py-1.5 text-xs focus:outline-none focus:ring-2 focus:ring-blue-500 dark:bg-gray-700 dark:text-gray-100 dark:placeholder-gray-400"
        />
      </div>
      <div className="px-2 py-1.5 border-b border-gray-200 dark:border-gray-700 flex gap-1" role="group" aria-label="Filter by origin">
        {ORIGIN_FILTERS.map(f => (
          <button
            key={f.value}
            onClick={() => setOriginFilter(f.value)}
            aria-pressed={originFilter === f.value}
            className={`flex-1 text-[11px] px-2 py-1 rounded-md transition-colors ${
              originFilter === f.value
                ? 'bg-blue-600 text-white font-medium'
                : 'text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700'
            }`}
          >
            {f.label}
          </button>
        ))}
      </div>
      <div className="flex-1 overflow-y-auto overscroll-contain">
        {chats.length === 0 && (
          <div className="text-xs text-gray-400 dark:text-gray-500 text-center py-6 px-3">No conversations yet</div>
        )}
        {chats.length > 0 && filtered.length === 0 && (
          <div className="text-xs text-gray-400 dark:text-gray-500 text-center py-6 px-3">No matches</div>
        )}
        {filtered.map(s => (
          <div
            key={s.id}
            onClick={() => onSelect(s.id)}
            className={`group relative flex flex-col px-3 py-2.5 cursor-pointer border-b border-gray-100 dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors ${activeChatId === s.id ? 'bg-blue-50 dark:bg-blue-900/30' : ''}`}
          >
            <span title={s.title || 'New chat'} className="block pr-6">
              <span className={`text-sm truncate block ${activeChatId === s.id ? 'text-blue-700 dark:text-blue-400 font-medium' : 'text-gray-800 dark:text-gray-100'}`}>
                {s.title || 'New chat'}
              </span>
            </span>
            {/* Badge row below the title: always rendered (even with no badges)
                so every row reserves the same vertical space and stays aligned.
                Repo + issue/PR badges link to GitHub; stopPropagation so a click
                opens the link instead of selecting the chat. */}
            <div className="flex items-center gap-1 h-4 mt-0.5 pr-6 overflow-hidden">
              {isGithubChat(s) && (() => {
                const ref = s.github_url ? githubRef(s.github_url) : null
                const chip =
                  'flex-shrink-0 text-[9px] font-semibold tracking-wide px-1 py-0.5 rounded bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400 hover:text-blue-600 dark:hover:text-blue-400 hover:underline'
                return (
                  <>
                    {s.github_repo && (
                      <a
                        href={`https://github.com/${s.github_repo}`}
                        target="_blank"
                        rel="noopener noreferrer"
                        onClick={e => e.stopPropagation()}
                        title={s.github_repo}
                        className={`${chip} truncate max-w-[120px]`}
                      >
                        {s.github_repo}
                      </a>
                    )}
                    {ref && (
                      <a
                        href={s.github_url!}
                        target="_blank"
                        rel="noopener noreferrer"
                        onClick={e => e.stopPropagation()}
                        title={ref.label}
                        className={chip}
                      >
                        {ref.label}
                      </a>
                    )}
                    {!s.github_repo && !ref && (
                      <span className={chip} aria-label="GitHub-originated chat">
                        GH
                      </span>
                    )}
                  </>
                )
              })()}
            </div>
            <span className="text-xs text-gray-400 dark:text-gray-500 mt-0.5">{relativeDate(s.updated_at)}</span>
            <button
              onClick={e => onDelete(s.id, e)}
              aria-label="Delete chat"
              className="absolute right-2 top-1/2 -translate-y-1/2 opacity-0 group-hover:opacity-100 text-gray-400 hover:text-red-500 transition-opacity p-1 rounded"
            >
              ×
            </button>
          </div>
        ))}
      </div>
    </div>
  )
}
