import type { ChatSummary } from '../api'
import { isGithubChat, parseGithubRef } from '../lib/github'
import { computeFacets, filterChats, parseFilterState, serializeFilterState, type SelectedFacets } from '../lib/chatFilters'
import { paletteClasses } from '../lib/colorHash'
import { FilterPanel } from './FilterPanel'
import { StatusDot } from './StatusDot'
import { navigate, useSearch } from '../router'

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
  // #736: the chat list is server-paginated - a `next_page_token` means more
  // chats exist beyond what's loaded.
  hasMoreChats?: boolean
  onLoadMoreChats?: () => void
  loadingMoreChats?: boolean
}

export function ChatList({ chats, activeChatId, open, onSelect, onNewChat, onDelete, onCloseMobile, hasMoreChats, onLoadMoreChats, loadingMoreChats }: ChatListProps) {
  const search = useSearch()
  const filterState = parseFilterState(search)
  const { q, selected } = filterState

  function setFilterState(next: { q?: string; selected?: SelectedFacets }) {
    const state = { q: next.q ?? q, selected: next.selected ?? selected }
    const qs = serializeFilterState(state)
    navigate(window.location.pathname + (qs ? `?${qs}` : ''), { replace: true })
  }

  function toggleFacet(facetKey: string, value: string) {
    const current = selected[facetKey] ?? []
    const next = current.includes(value) ? current.filter(v => v !== value) : [...current, value]
    setFilterState({ selected: { ...selected, [facetKey]: next } })
  }

  const facets = computeFacets(chats)
  const filtered = filterChats(chats, filterState)

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
      <div className="p-2 border-b border-gray-200 dark:border-gray-700 flex items-center gap-1.5">
        <input
          type="search"
          value={q}
          onChange={e => setFilterState({ q: e.target.value })}
          placeholder="Search chats…"
          aria-label="Search chats"
          className="flex-1 min-w-0 rounded-lg border border-gray-300 dark:border-gray-600 px-3 py-1.5 text-xs focus:outline-none focus:ring-2 focus:ring-blue-500 dark:bg-gray-700 dark:text-gray-100 dark:placeholder-gray-400"
        />
        <FilterPanel
          facets={facets}
          selected={selected}
          onToggle={toggleFacet}
          onClear={() => setFilterState({ selected: {} })}
        />
      </div>
      <div className="flex-1 overflow-y-auto overscroll-contain">
        {chats.length === 0 && (
          <div className="text-xs text-gray-400 dark:text-gray-500 text-center py-6 px-3">No conversations yet</div>
        )}
        {chats.length > 0 && filtered.length === 0 && (
          <div className="text-xs text-gray-400 dark:text-gray-500 text-center py-6 px-3">No matches</div>
        )}
        {filtered.map(s => {
          const ref = parseGithubRef(s)
          return (
            <div
              key={s.id}
              onClick={() => onSelect(s.id)}
              className={`group relative flex flex-col px-3 py-2.5 cursor-pointer border-b border-gray-100 dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors ${activeChatId === s.id ? 'bg-blue-50 dark:bg-blue-900/30' : ''}`}
            >
              <span title={s.title || 'New chat'} className="flex items-center pr-6">
                <StatusDot status={s.status} className="mr-1.5" variant="chat" />
                <span className={`text-sm truncate block ${activeChatId === s.id ? 'text-blue-700 dark:text-blue-400 font-medium' : 'text-gray-800 dark:text-gray-100'}`}>
                  {s.title || 'New chat'}
                </span>
              </span>
              {/* Badge row below the title: always rendered (even with no badges)
                  so every row reserves the same vertical space and stays aligned.
                  Repo/Issue/PR badges link out to GitHub - filtering by repo/type
                  lives entirely in the FilterPanel popover, not on the badges. */}
              <div className="flex items-center gap-1 h-4 mt-0.5 pr-6">
                {isGithubChat(s) && ref && (
                  <a
                    href={`https://github.com/${ref.repo}`}
                    target="_blank"
                    rel="noopener noreferrer"
                    onClick={e => e.stopPropagation()}
                    title={ref.repo}
                    // #746 item 10: a deterministic (hashed, not assignment-order)
                    // colour per repo, so it's the same colour every reload and
                    // across every chat - always paired with the repo NAME text,
                    // never colour alone.
                    className={`flex-shrink-0 max-w-[7rem] truncate text-[9px] font-semibold tracking-wide px-1 py-0.5 rounded hover:underline ${paletteClasses(ref.repo)}`}
                  >
                    {ref.repo}
                  </a>
                )}
                {ref && s.github_url && (
                  <a
                    href={s.github_url}
                    target="_blank"
                    rel="noopener noreferrer"
                    onClick={e => e.stopPropagation()}
                    title={ref.kind === 'pr' ? `Pull request #${ref.number}` : `Issue #${ref.number}`}
                    className="flex-shrink-0 text-[9px] font-semibold tracking-wide px-1 py-0.5 rounded bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400 hover:text-blue-600 dark:hover:text-blue-400 hover:underline"
                  >
                    {ref.kind === 'pr' ? 'PR' : 'Issue'} #{ref.number}
                  </a>
                )}
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
          )
        })}
        {hasMoreChats && (
          <button
            onClick={onLoadMoreChats}
            disabled={loadingMoreChats}
            className="w-full text-xs text-center py-2.5 text-blue-600 dark:text-blue-400 hover:bg-gray-50 dark:hover:bg-gray-700 disabled:opacity-50 transition-colors"
          >
            {loadingMoreChats ? 'Loading…' : 'Load more'}
          </button>
        )}
      </div>
    </div>
  )
}
