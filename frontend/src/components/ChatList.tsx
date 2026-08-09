import { useMemo, useState } from 'react'
import type { ChatSummary } from '../api'
import { isGithubChat, parseGithubRef } from '../lib/github'
import { computeFacets, filterChats, parseFilterState, serializeFilterState, type SelectedFacets } from '../lib/chatFilters'
import { paletteClasses } from '../lib/colorHash'
import { FilterPanel } from './FilterPanel'
import { StatusDot } from './StatusDot'
import { navigate, useSearch } from '../router'

export function githubStateBadgeClass(state: string): string {
  switch (state) {
    case 'open': return 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400'
    case 'closed': return 'bg-red-100 text-red-600 dark:bg-red-900/30 dark:text-red-400'
    case 'merged': return 'bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-400'
    case 'draft': return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-500'
    default: return ''
  }
}

export function githubStateLabel(state: string): string {
  const map: Record<string, string> = {
    open: '◉ open',
    closed: '✕ closed',
    merged: '✓ merged',
    draft: '⊘ draft',
  }
  return map[state] ?? ''
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
  // #736: the chat list is server-paginated - a `next_page_token` means more
  // chats exist beyond what's loaded.
  hasMoreChats?: boolean
  onLoadMoreChats?: () => void
  loadingMoreChats?: boolean
  onArchive?: (chatId: string) => void
  onUnarchive?: (chatId: string) => void
  // #722: fired when the collapsed Archived section expand button is clicked -
  // tells the parent to refetch with showArchived=true so archived chats load.
  onShowArchived?: () => void
}

// ChatRow renders a single chat row. The × is a two-stage trash: on an active
// row it archives, on an archived row it hard-deletes; `archived` also gates
// the Restore control. Reusable by both the active groups and the archived section.
function ChatRow({
  s,
  activeChatId,
  onSelect,
  onDelete,
  archived,
  onArchive,
  onUnarchive,
}: {
  s: ChatSummary
  activeChatId: string | null
  onSelect: (id: string) => void
  onDelete: (id: string, e: React.MouseEvent) => void
  archived?: boolean
  onArchive?: (chatId: string) => void
  onUnarchive?: (chatId: string) => void
}) {
  const ref = parseGithubRef(s)

  // The × is a two-stage trash: archive first (reversible), hard-delete second.
  // Only the irreversible path needs a confirm.
  function handleTrashClick(e: React.MouseEvent) {
    e.stopPropagation()
    if (archived) {
      if (window.confirm(`Permanently delete "${s.title || 'New chat'}"? This can't be undone.`)) {
        onDelete(s.id, e)
      }
      return
    }
    onArchive?.(s.id)
  }

  return (
    <div
      className={`group relative flex flex-col px-3 py-2.5 cursor-pointer border-b border-gray-100 dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors ${activeChatId === s.id ? 'bg-blue-50 dark:bg-blue-900/30' : ''}`}
      onClick={() => onSelect(s.id)}
    >
      <span title={s.title || 'New chat'} className="flex items-center pr-6">
        <StatusDot status={s.status} className="mr-1.5" variant="chat" />
        <span className={`text-sm truncate block ${activeChatId === s.id ? 'text-blue-700 dark:text-blue-400 font-medium' : 'text-gray-800 dark:text-gray-100'}`}>
          {s.title || 'New chat'}
        </span>
      </span>
      {/* Badge row below the title: always rendered so every row reserves
          the same vertical space and stays aligned. Repo/Issue/PR badges
          link out to GitHub - filtering by repo/type lives in the FilterPanel. */}
      <div className="flex items-center gap-1 h-4 mt-0.5 pr-6">
        {isGithubChat(s) && ref && (
          <a
            href={`https://github.com/${ref.repo}`}
            target="_blank"
            rel="noopener noreferrer"
            onClick={e => e.stopPropagation()}
            title={ref.repo}
            className={`flex-shrink-0 max-w-[7rem] truncate text-[9px] font-semibold tracking-wide px-1 py-0.5 rounded hover:underline ${paletteClasses(ref.repo)}`}
          >
            {ref.repo.slice(ref.repo.indexOf('/') + 1)}
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
        {s.github_state && (
          <span
            className={`flex-shrink-0 text-[9px] font-semibold tracking-wide px-1 py-0.5 rounded ${githubStateBadgeClass(s.github_state)}`}
            title={s.github_state}
          >
            {githubStateLabel(s.github_state)}
          </span>
        )}
      </div>
      <span className="text-xs text-gray-400 dark:text-gray-500 mt-0.5">{relativeDate(s.updated_at)}</span>
      {/* Archived rows get a real restore control (not hover-only, not tiny text) -
          it's the only way back once a chat has left the active list. */}
      {archived && onUnarchive && (
        <button
          onClick={e => { e.stopPropagation(); onUnarchive(s.id) }}
          aria-label="Unarchive chat"
          title="Unarchive chat"
          className="mt-1.5 self-start inline-flex items-center gap-1 text-xs font-medium px-2.5 py-1 rounded-md bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-300 hover:bg-blue-100 dark:hover:bg-blue-900/40 hover:text-blue-700 dark:hover:text-blue-400 transition-colors"
        >
          <span aria-hidden="true">↺</span> Restore
        </button>
      )}
      <button
        onClick={handleTrashClick}
        aria-label={archived ? 'Delete chat permanently' : 'Archive chat'}
        title={archived ? 'Delete chat permanently' : 'Archive chat'}
        className="absolute right-2 top-1/2 -translate-y-1/2 opacity-0 group-hover:opacity-100 text-gray-400 hover:text-red-500 transition-opacity p-1 rounded"
      >
        ×
      </button>
    </div>
  )
}

export function ChatList({ chats, activeChatId, open, onSelect, onNewChat, onDelete, onCloseMobile, hasMoreChats, onLoadMoreChats, loadingMoreChats, onArchive, onUnarchive, onShowArchived }: ChatListProps) {
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

  // #722 group the sidebar by run state: running/queued first (no header),
  // active chats below, archived in a collapsed section at bottom. Empty
  // groups render nothing. Archived toggle must NOT update UpdatedAt so
  // archiving never re-orders the list.
  const [archivedExpanded, setArchivedExpanded] = useState(false)

  const runningQueued = useMemo<ChatSummary[]>(() => {
    return filtered.filter(c => c.archived !== true && (c.status === 'running' || c.status === 'queued'))
  }, [filtered])

  const active = useMemo<ChatSummary[]>(() => {
    return filtered.filter(c => c.archived !== true && c.status !== 'running' && c.status !== 'queued')
  }, [filtered])

  const archived = useMemo<ChatSummary[]>(() => {
    const found = filtered.filter(c => c.archived === true)
    // Sort by updated_at desc when showing all archived.
    return [...found].sort((a, b) => new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime())
  }, [filtered])


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
        {(runningQueued.length === 0 && active.length === 0 && archived.length === 0) && chats.length === 0 && (
          <div className="text-xs text-gray-400 dark:text-gray-500 text-center py-6 px-3">No conversations yet</div>
        )}
        {(runningQueued.length === 0 && active.length === 0 && archived.length === 0) && chats.length > 0 && (
          <div className="text-xs text-gray-400 dark:text-gray-500 text-center py-6 px-3">No matches</div>
        )}

        {/* Active groups: running/queued then idle — empty groups render nothing */}
        {runningQueued.map(s => (
          <ChatRow key={s.id} s={s} activeChatId={activeChatId} onSelect={onSelect} onDelete={onDelete} onArchive={onArchive} />
        ))}
        {active.map(s => (
          <ChatRow key={s.id} s={s} activeChatId={activeChatId} onSelect={onSelect} onDelete={onDelete} onArchive={onArchive} />
        ))}

        {/* Archived section: collapsed by default, expands to show archived rows */}
        {archived.length > 0 && (
          <div>
            <button
              onClick={() => { setArchivedExpanded(prev => !prev); onShowArchived?.() }}
              className="w-full flex items-center gap-2 px-3 py-1.5 text-xs font-medium text-gray-500 dark:text-gray-400 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors border-t border-gray-200 dark:border-gray-700"
              aria-expanded={archivedExpanded}
            >
              <span className={`transition-transform inline-block ${archivedExpanded ? 'rotate-90' : ''}`}>›</span>
              Archived ({archived.length})
            </button>
            {archivedExpanded && archived.map(s => (
              <ChatRow key={s.id} s={s} activeChatId={activeChatId} onSelect={onSelect} onDelete={onDelete} onUnarchive={onUnarchive} archived />
            ))}
          </div>
        )}

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
