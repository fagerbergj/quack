import { useEffect, useMemo, useRef, useState } from 'react'
import type { ChatSummary } from '../api'
import { isGithubChat, parseGithubRef, type GithubRef } from '../lib/github'
import { computeFacets, filterChats, parseFilterState, serializeFilterState, type SelectedFacets } from '../lib/chatFilters'
import { paletteClasses, TONE } from '../lib/colorHash'
import { FilterPanel } from './FilterPanel'
import { StatusDot } from './StatusDot'
import { navigate, useSearch } from '../router'
import { useMediaQuery } from '../hooks/useMediaQuery'
import { useDrawer } from '../hooks/useDrawer'
import { Icon, type IconName } from './Icon'

const GITHUB_STATE_TONE = new Map<string, string>([['open', TONE.green], ['closed', TONE.red], ['merged', TONE.purple], ['draft', TONE.yellow]])

export function githubStateBadgeClass(state: string): string {
  return GITHUB_STATE_TONE.get(state) ?? ''
}

// GitHub's colours for an origin badge, but never for 'draft': an extension's own badge
// vocabulary (a doc's "draft" revision label) must not be guessed at, so it stays neutral.
export function originBadgeClass(badge: string): string {
  return (badge !== 'draft' && GITHUB_STATE_TONE.get(badge)) || TONE.gray
}

// Icon plus colour convey the state, never colour alone (WCAG 1.4.1).
export function githubStateIcon(state: string): IconName | undefined {
  const map: Record<string, IconName> = {
    open: 'dot',
    closed: 'close',
    merged: 'check',
    draft: 'edit',
  }
  return map[state]
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
  // Server-scoped to status=active, so this list and archivedChats page independently.
  chats: ChatSummary[]
  activeChatId: string | null
  open: boolean
  onSelect: (id: string) => void
  onNewChat: () => void
  onDelete: (id: string, e: React.MouseEvent) => void
  onCloseMobile: () => void
  hasMoreChats?: boolean
  onLoadMoreChats?: () => void
  loadingMoreChats?: boolean
  onArchive?: (chatId: string) => void
  onUnarchive?: (chatId: string) => void
  // Fetched on first expand of the Archived section; undefined means not yet loaded.
  archivedChats?: ChatSummary[]
  hasMoreArchivedChats?: boolean
  onLoadMoreArchivedChats?: () => void
  loadingMoreArchivedChats?: boolean
  // Fires on expand only, never on collapse.
  onExpandArchived?: () => void
}

function ChatBadges({ s, githubRef }: { s: ChatSummary; githubRef: GithubRef | undefined }) {
  const ref = githubRef
  return (
    <>
      {isGithubChat(s) && ref && (
        <a
          href={`https://github.com/${ref.repo}`}
          target="_blank"
          rel="noopener noreferrer"
          onClick={e => e.stopPropagation()}
          title={ref.repo}
          className={`flex-shrink-0 max-w-[7rem] truncate text-[11px] font-semibold tracking-wide px-1 py-1 rounded hover:underline ${paletteClasses(ref.repo)}`}
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
          className={`flex-shrink-0 text-[11px] font-semibold tracking-wide px-1 py-1 rounded ${TONE.gray} hover:text-blue-600 dark:hover:text-blue-400 hover:underline`}
        >
          {ref.kind === 'pr' ? 'PR' : 'Issue'} #{ref.number}
        </a>
      )}
      {s.github_state && (
        <span
          className={`flex-shrink-0 inline-flex items-center gap-0.5 text-[11px] font-semibold tracking-wide px-1 py-1 rounded ${githubStateBadgeClass(s.github_state)}`}
          title={s.github_state}
        >
          {githubStateIcon(s.github_state) && <Icon name={githubStateIcon(s.github_state)!} className="w-2.5 h-2.5" />}
          {GITHUB_STATE_TONE.has(s.github_state) && s.github_state}
        </span>
      )}
      {/* Extension-dispatched chats. GitHub keeps its dedicated fields above until it stamps origin itself. */}
      {s.origin && (
        <>
          {s.origin.href ? (
            <a
              href={s.origin.href}
              target="_blank"
              rel="noopener noreferrer"
              onClick={e => e.stopPropagation()}
              title={s.origin.label}
              className={`flex-shrink-0 max-w-[7rem] truncate text-[11px] font-semibold tracking-wide px-1 py-1 rounded hover:underline ${paletteClasses(s.origin.extension)}`}
            >
              {s.origin.label}
            </a>
          ) : (
            <span
              title={s.origin.label}
              className={`flex-shrink-0 max-w-[7rem] truncate text-[11px] font-semibold tracking-wide px-1 py-1 rounded ${paletteClasses(s.origin.extension)}`}
            >
              {s.origin.label}
            </span>
          )}
          {s.origin.badge && (
            <span
              className={`flex-shrink-0 text-[11px] font-semibold tracking-wide px-1 py-1 rounded ${originBadgeClass(s.origin.badge)}`}
              title={s.origin.badge}
            >
              {s.origin.badge}
            </span>
          )}
        </>
      )}
    </>
  )
}

// Absolutely positioned, not in flow, so it never grows the row's height. Always visible: touch has no hover.
function ChatRowMenu({ s, menuRef, btnRef, menuOpen, onToggle, archived, onArchive, onUnarchive, onDelete }: {
  s: ChatSummary
  menuRef: React.RefObject<HTMLDivElement | null>
  btnRef: React.RefObject<HTMLButtonElement | null>
  menuOpen: boolean
  onToggle: () => void
  archived?: boolean
  onArchive?: (chatId: string) => void
  onUnarchive?: (chatId: string) => void
  onDelete: (e: React.MouseEvent) => void
}) {
  return (
    <div ref={menuRef} className="absolute right-0 top-0">
      <button
        ref={btnRef}
        onClick={e => { e.stopPropagation(); onToggle() }}
        aria-label="Chat actions"
        aria-haspopup="menu"
        aria-expanded={menuOpen}
        title="Chat actions"
        className="inline-flex items-center justify-center min-w-[44px] min-h-[44px] rounded-md text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
      >
        <Icon name="more_vert" className="w-5 h-5" />
      </button>
      {menuOpen && (
        <div
          role="menu"
          className="absolute right-0 top-full mt-1 z-10 min-w-[8rem] rounded-md border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 shadow-lg py-1"
        >
          {!archived && (
            <button
              role="menuitem"
              onClick={e => { e.stopPropagation(); onToggle(); onArchive?.(s.id) }}
              aria-label="Archive chat"
              title="Archive chat"
              className="w-full flex items-center gap-1.5 text-left px-3 py-1.5 min-h-[44px] medium:min-h-0 text-xs font-medium text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors"
            >
              <Icon name="archive" className="w-3.5 h-3.5" /> Archive
            </button>
          )}
          {archived && onUnarchive && (
            <button
              role="menuitem"
              onClick={e => { e.stopPropagation(); onToggle(); onUnarchive(s.id) }}
              aria-label="Unarchive chat"
              title="Unarchive chat"
              className="w-full flex items-center gap-1.5 text-left px-3 py-1.5 min-h-[44px] medium:min-h-0 text-xs font-medium text-gray-600 dark:text-gray-300 hover:bg-blue-50 dark:hover:bg-blue-900/40 hover:text-blue-700 dark:hover:text-blue-400 transition-colors"
            >
              <Icon name="history" className="w-3.5 h-3.5" /> Restore
            </button>
          )}
          {archived && (
            <button
              role="menuitem"
              onClick={onDelete}
              aria-label="Delete chat permanently"
              title="Delete chat permanently"
              className="w-full flex items-center gap-1.5 text-left px-3 py-1.5 min-h-[44px] medium:min-h-0 text-xs font-medium text-red-500 dark:text-red-400 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors"
            >
              <Icon name="delete" className="w-3.5 h-3.5" /> Delete
            </button>
          )}
        </div>
      )}
    </div>
  )
}

// Active rows' menu holds Archive (reversible); archived rows' holds Restore and permanent Delete.
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
  const [menuOpen, setMenuOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)
  const btnRef = useRef<HTMLButtonElement>(null)
  // The menu unmounts on close; without an explicit return, keyboard users land on <body> (APG menu button).
  const close = () => { setMenuOpen(false); btnRef.current?.focus() }

  useEffect(() => {
    if (!menuOpen) return
    function onDocMouseDown(e: MouseEvent) {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) setMenuOpen(false)
    }
    function onDocKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') { close(); return }
      if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return
      const items = Array.from(menuRef.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])
      if (items.length === 0) return
      e.preventDefault()
      const idx = items.indexOf(document.activeElement as HTMLElement)
      const next = e.key === 'ArrowDown' ? (idx + 1) % items.length : (idx - 1 + items.length) % items.length
      items[next]?.focus()
    }
    document.addEventListener('mousedown', onDocMouseDown)
    document.addEventListener('keydown', onDocKeyDown)
    return () => {
      document.removeEventListener('mousedown', onDocMouseDown)
      document.removeEventListener('keydown', onDocKeyDown)
    }
  }, [menuOpen])

  useEffect(() => {
    if (!menuOpen) return
    menuRef.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus()
  }, [menuOpen])

  // Only the irreversible path needs a confirm.
  function handleDelete(e: React.MouseEvent) {
    e.stopPropagation()
    setMenuOpen(false)
    if (window.confirm(`Permanently delete "${s.title || 'New chat'}"? This can't be undone.`)) {
      onDelete(s.id, e)
    }
  }

  return (
    <div
      className={`group relative flex flex-col px-3 py-2.5 cursor-pointer border-b border-gray-100 dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors ${activeChatId === s.id ? 'bg-blue-50 dark:bg-blue-900/30' : ''}`}
      onClick={() => onSelect(s.id)}
    >
      <span title={s.title || 'New chat'} className="flex items-center pr-11">
        <StatusDot status={s.status} className="mr-1.5" variant="chat" />
        <span className={`text-sm truncate block ${activeChatId === s.id ? 'text-blue-700 dark:text-blue-400 font-medium' : 'text-gray-800 dark:text-gray-100'}`}>
          {s.title || 'New chat'}
        </span>
      </span>
      {/* Always rendered so every row reserves the same height and stays aligned. */}
      <div className="flex items-center gap-1 h-4 mt-0.5 pr-6">
        <ChatBadges s={s} githubRef={ref} />
      </div>
      <span className="text-xs text-gray-500 dark:text-gray-400 mt-0.5">{relativeDate(s.updated_at)}</span>
      <ChatRowMenu
        s={s}
        menuRef={menuRef}
        btnRef={btnRef}
        menuOpen={menuOpen}
        onToggle={() => setMenuOpen(o => !o)}
        archived={archived}
        onArchive={onArchive}
        onUnarchive={onUnarchive}
        onDelete={handleDelete}
      />
    </div>
  )
}

function mergeFilterState(q: string, selected: SelectedFacets, next: { q?: string; selected?: SelectedFacets }) {
  return { q: next.q ?? q, selected: next.selected ?? selected }
}

function withFacetToggled(selected: SelectedFacets, facetKey: string, value: string) {
  const current = selected[facetKey] ?? []
  return { ...selected, [facetKey]: current.includes(value) ? current.filter(v => v !== value) : [...current, value] }
}

function NoChats({ chats, listEmpty }: { chats: ChatSummary[]; listEmpty: boolean }) {
  if (!listEmpty) return null
  return <div className="text-xs text-gray-500 dark:text-gray-400 text-center py-6 px-3">{chats.length === 0 ? 'No conversations yet' : 'No matches'}</div>
}

// Always rendered, not gated on archived.length, so it is discoverable before its lazy first fetch.
function ArchivedSection({ archived, loaded, expanded, onToggle, activeChatId, onSelect, onDelete, onUnarchive, hasMore, loading, onLoadMore }: {
  archived: ChatSummary[]
  loaded: boolean
  expanded: boolean
  onToggle: () => void
  activeChatId: string | null
  onSelect: (id: string) => void
  onDelete: (id: string, e: React.MouseEvent) => void
  onUnarchive?: (chatId: string) => void
  hasMore?: boolean
  loading?: boolean
  onLoadMore?: () => void
}) {
  return (
    <div>
      <button
        onClick={onToggle}
        className="w-full flex items-center gap-2 px-3 py-1.5 text-xs font-medium text-gray-500 dark:text-gray-400 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors border-t border-gray-200 dark:border-gray-700"
        aria-expanded={expanded}
      >
        <span aria-hidden="true" className={`transition-transform inline-block ${expanded ? 'rotate-90' : ''}`}>›</span>
        Archived{loaded ? ` (${archived.length})` : ''}
      </button>
      {expanded && (
        <>
          {archived.length === 0 && (
            <div className="text-xs text-gray-500 dark:text-gray-400 text-center py-3 px-3">No archived chats</div>
          )}
          {archived.map(s => (
            <ChatRow key={s.id} s={s} activeChatId={activeChatId} onSelect={onSelect} onDelete={onDelete} onUnarchive={onUnarchive} archived />
          ))}
          {hasMore && (
            <button
              onClick={onLoadMore}
              disabled={loading}
              aria-label="Load more archived chats"
              className="w-full min-h-[44px] text-xs text-center text-blue-600 dark:text-blue-400 hover:bg-gray-50 dark:hover:bg-gray-700 disabled:opacity-50 transition-colors"
            >
              {loading ? 'Loading…' : 'Load more'}
            </button>
          )}
        </>
      )}
    </div>
  )
}

export function ChatList({ chats, activeChatId, open, onSelect, onNewChat, onDelete, onCloseMobile, hasMoreChats, onLoadMoreChats, loadingMoreChats, onArchive, onUnarchive, archivedChats, hasMoreArchivedChats, onLoadMoreArchivedChats, loadingMoreArchivedChats, onExpandArchived }: ChatListProps) {
  const search = useSearch()
  const filterState = parseFilterState(search)
  const { q, selected } = filterState

  function setFilterState(next: { q?: string; selected?: SelectedFacets }) {
    const state = mergeFilterState(q, selected, next)
    const qs = serializeFilterState(state)
    navigate(window.location.pathname + '?' + qs, { replace: true })
  }

  function toggleFacet(facetKey: string, value: string) {
    setFilterState({ selected: withFacetToggled(selected, facetKey, value) })
  }

  const facets = computeFacets(chats)
  const filtered = filterChats(chats, filterState)

  const [archivedExpanded, setArchivedExpanded] = useState(false)

  function toggleArchived() {
    setArchivedExpanded(prev => {
      const next = !prev
      if (next) onExpandArchived?.()
      return next
    })
  }

  const running = useMemo<ChatSummary[]>(() => {
    return filtered.filter(c => c.status === 'running')
  }, [filtered])

  const active = useMemo<ChatSummary[]>(() => {
    return filtered.filter(c => c.status !== 'running')
  }, [filtered])

  // archivedChats is already server-scoped to status=archived; only search/facets apply here.
  const archived = filterChats(archivedChats ?? [], filterState)

  // Must match the `medium` (600px) breakpoint in `fixed medium:static` below, so the drawer a11y wiring
  // is armed at every width where the panel is off-canvas.
  const offCanvas = useMediaQuery('(max-width: 599px)')
  const panelRef = useDrawer(open && offCanvas, onCloseMobile)
  const dialogAria = offCanvas && open

  return (
    <div
      ref={panelRef}
      role={dialogAria ? 'dialog' : undefined}
      aria-modal={dialogAria ? true : undefined}
      aria-label={dialogAria ? 'Chat list' : undefined}
      className={`
      fixed medium:static inset-y-0 left-0 z-40
      h-dvh w-[250px] flex-shrink-0 flex flex-col
      border-r border-gray-200 dark:border-gray-700
      bg-white dark:bg-gray-800
      transition-transform duration-200
      medium:translate-x-0
      ${open ? 'translate-x-0' : '-translate-x-full medium:translate-x-0'}
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
          className="medium:hidden text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 p-1.5 rounded transition-colors min-w-[44px] min-h-[44px] flex items-center justify-center"
          aria-label="Close chat list"
        >
          <Icon name="close" className="w-4 h-4" />
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
      <div className="flex-1 overflow-y-auto overscroll-contain chat-list-scroll">
        <NoChats chats={chats} listEmpty={running.length === 0 && active.length === 0 && archived.length === 0} />

        {running.map(s => (
          <ChatRow key={s.id} s={s} activeChatId={activeChatId} onSelect={onSelect} onDelete={onDelete} onArchive={onArchive} />
        ))}
        {active.map(s => (
          <ChatRow key={s.id} s={s} activeChatId={activeChatId} onSelect={onSelect} onDelete={onDelete} onArchive={onArchive} />
        ))}

        <ArchivedSection
          archived={archived}
          loaded={archivedChats !== undefined}
          expanded={archivedExpanded}
          onToggle={toggleArchived}
          activeChatId={activeChatId}
          onSelect={onSelect}
          onDelete={onDelete}
          onUnarchive={onUnarchive}
          hasMore={hasMoreArchivedChats}
          loading={loadingMoreArchivedChats}
          onLoadMore={onLoadMoreArchivedChats}
        />

        {hasMoreChats && (
          <button
            onClick={onLoadMoreChats}
            disabled={loadingMoreChats}
            className="w-full min-h-[44px] text-xs text-center text-blue-600 dark:text-blue-400 hover:bg-gray-50 dark:hover:bg-gray-700 disabled:opacity-50 transition-colors"
          >
            {loadingMoreChats ? 'Loading…' : 'Load more'}
          </button>
        )}
      </div>
    </div>
  )
}
