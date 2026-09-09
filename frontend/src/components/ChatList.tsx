import { useEffect, useMemo, useRef, useState } from 'react'
import type { ChatSummary } from '../api'
import { isGithubChat, parseGithubRef } from '../lib/github'
import { computeFacets, filterChats, parseFilterState, serializeFilterState, type SelectedFacets } from '../lib/chatFilters'
import { paletteClasses } from '../lib/colorHash'
import { FilterPanel } from './FilterPanel'
import { StatusDot } from './StatusDot'
import { navigate, useSearch } from '../router'
import { useMediaQuery } from '../hooks/useMediaQuery'
import { useDrawer } from '../hooks/useDrawer'
import { Icon, type IconName } from './Icon'

export function githubStateBadgeClass(state: string): string {
  switch (state) {
    case 'open': return 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400'
    case 'closed': return 'bg-red-100 text-red-600 dark:bg-red-900/30 dark:text-red-400'
    case 'merged': return 'bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-400'
    case 'draft': return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-500'
    default: return ''
  }
}

// originBadgeClass mirrors GitHub's own state colors for the generic origin
// badge (#832), but only for these three exact values - an extension's own
// badge vocabulary (e.g. "draft", a doc's revision label) is unknown to us
// and must not be guessed at, so it keeps the neutral chip below.
export function originBadgeClass(badge: string): string {
  switch (badge) {
    case 'open': return 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400'
    case 'merged': return 'bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-400'
    case 'closed': return 'bg-red-100 text-red-600 dark:bg-red-900/30 dark:text-red-400'
    default: return 'bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400'
  }
}

export function githubStateLabel(state: string): string {
  const map: Record<string, string> = {
    open: 'open',
    closed: 'closed',
    merged: 'merged',
    draft: 'draft',
  }
  return map[state] ?? ''
}

// githubStateIcon is the Material icon paired with githubStateLabel's text -
// the badge's color (githubStateBadgeClass) plus this icon convey the state,
// not color alone (WCAG 1.4.1).
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
  // #809: server-scoped to active chats only (status=active) - never carries
  // archived rows, so this list and archivedChats page independently.
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
  // #809: the Archived section's own scoped (status=archived) list, fetched
  // only once the section is first expanded - absent/undefined means not yet loaded.
  archivedChats?: ChatSummary[]
  hasMoreArchivedChats?: boolean
  onLoadMoreArchivedChats?: () => void
  loadingMoreArchivedChats?: boolean
  // Fired the moment the Archived section expands (not on collapse) - tells
  // the parent to fetch archivedChats if it hasn't already.
  onExpandArchived?: () => void
}

// ChatRow renders a single chat row. Every row has exactly one always-visible
// kebab (#1319): an active row's menu holds Archive (reversible); an archived
// row's holds Restore and permanent Delete. Reusable by both the active
// groups and the archived section.
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
  // The menu unmounts on close, so without an explicit return a keyboard or
  // screen-reader user is dropped on <body> (APG menu button pattern, same as
  // DagNode's NodeMenu).
  const close = () => { setMenuOpen(false); btnRef.current?.focus() }

  // Outside click/tap closes; Escape closes and returns focus to the kebab;
  // arrow keys move between items.
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

  // Move focus onto the first item when the menu opens (keyboard Enter/Space
  // on the trigger, or a mouse click - refocusing on click is harmless).
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
            className={`flex-shrink-0 inline-flex items-center gap-0.5 text-[9px] font-semibold tracking-wide px-1 py-0.5 rounded ${githubStateBadgeClass(s.github_state)}`}
            title={s.github_state}
          >
            {githubStateIcon(s.github_state) && <Icon name={githubStateIcon(s.github_state)!} className="w-2.5 h-2.5" />}
            {githubStateLabel(s.github_state)}
          </span>
        )}
        {/* Generic origin chip (extension-dispatched chats, e.g. reMarkable) -
            label chip, optional badge, subject link. GitHub stays on its own
            dedicated fields above until it migrates to stamping origin itself. */}
        {s.origin && (
          <>
            {s.origin.href ? (
              <a
                href={s.origin.href}
                target="_blank"
                rel="noopener noreferrer"
                onClick={e => e.stopPropagation()}
                title={s.origin.label}
                className={`flex-shrink-0 max-w-[7rem] truncate text-[9px] font-semibold tracking-wide px-1 py-0.5 rounded hover:underline ${paletteClasses(s.origin.extension)}`}
              >
                {s.origin.label}
              </a>
            ) : (
              <span
                title={s.origin.label}
                className={`flex-shrink-0 max-w-[7rem] truncate text-[9px] font-semibold tracking-wide px-1 py-0.5 rounded ${paletteClasses(s.origin.extension)}`}
              >
                {s.origin.label}
              </span>
            )}
            {s.origin.badge && (
              <span
                className={`flex-shrink-0 text-[9px] font-semibold tracking-wide px-1 py-0.5 rounded ${originBadgeClass(s.origin.badge)}`}
                title={s.origin.badge}
              >
                {s.origin.badge}
              </span>
            )}
          </>
        )}
      </div>
      <span className="text-xs text-gray-400 dark:text-gray-500 mt-0.5">{relativeDate(s.updated_at)}</span>
      {/* Every row's one action point (#1319 - archive/delete both live here,
          two clicks instead of a bare one-tap control). Absolutely positioned
          in the top-right corner, NOT in flow, so it never grows the row's
          height. Always visible: touch has no hover to reveal it. */}
      <div ref={menuRef} className="absolute right-0 top-0">
        <button
          ref={btnRef}
          onClick={e => { e.stopPropagation(); setMenuOpen(o => !o) }}
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
                onClick={e => { e.stopPropagation(); setMenuOpen(false); onArchive?.(s.id) }}
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
                onClick={e => { e.stopPropagation(); setMenuOpen(false); onUnarchive(s.id) }}
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
                onClick={handleDelete}
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
    </div>
  )
}

export function ChatList({ chats, activeChatId, open, onSelect, onNewChat, onDelete, onCloseMobile, hasMoreChats, onLoadMoreChats, loadingMoreChats, onArchive, onUnarchive, archivedChats, hasMoreArchivedChats, onLoadMoreArchivedChats, loadingMoreArchivedChats, onExpandArchived }: ChatListProps) {
  const search = useSearch()
  const filterState = parseFilterState(search)
  const { q, selected } = filterState

  function setFilterState(next: { q?: string; selected?: SelectedFacets }) {
    const state = { q: next.q ?? q, selected: next.selected ?? selected }
    const qs = serializeFilterState(state)
    navigate(window.location.pathname + '?' + qs, { replace: true })
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
  // groups render nothing.
  const [archivedExpanded, setArchivedExpanded] = useState(false)

  function toggleArchived() {
    setArchivedExpanded(prev => {
      const next = !prev
      if (next) onExpandArchived?.() // #809: fetch archived on expand only, never on collapse
      return next
    })
  }

  const runningQueued = useMemo<ChatSummary[]>(() => {
    return filtered.filter(c => c.status === 'running' || c.status === 'queued')
  }, [filtered])

  const active = useMemo<ChatSummary[]>(() => {
    return filtered.filter(c => c.status !== 'running' && c.status !== 'queued')
  }, [filtered])

  // #809: archivedChats is server-scoped (status=archived) already - no
  // client-side archived filter or re-sort needed, just the shared search/facet filter.
  const archived = filterChats(archivedChats ?? [], filterState)

  // Off-canvas below `medium` (600px, `fixed medium:static` below - the one
  // compact/expanded line the whole app switches on), persistent alongside
  // the chat pane above it (#1131). The drawer a11y wiring (Esc, focus trap,
  // scroll lock, return focus) is armed on that same query so there's no
  // width where the panel is off-canvas but the wiring is dark.
  const offCanvas = useMediaQuery('(max-width: 599px)')
  const panelRef = useDrawer(open && offCanvas, onCloseMobile)

  return (
    <div
      ref={panelRef}
      role={offCanvas && open ? 'dialog' : undefined}
      aria-modal={offCanvas && open ? true : undefined}
      aria-label={offCanvas && open ? 'Chat list' : undefined}
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

        {/* Archived section: collapsed by default. Always rendered (not gated
            on archived.length) so it's discoverable before its own list has
            ever been fetched - #809 loads it lazily on first expand. */}
        <div>
          <button
            onClick={toggleArchived}
            className="w-full flex items-center gap-2 px-3 py-1.5 text-xs font-medium text-gray-500 dark:text-gray-400 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors border-t border-gray-200 dark:border-gray-700"
            aria-expanded={archivedExpanded}
          >
            <span className={`transition-transform inline-block ${archivedExpanded ? 'rotate-90' : ''}`}>›</span>
            Archived{archivedChats !== undefined ? ` (${archived.length})` : ''}
          </button>
          {archivedExpanded && (
            <>
              {archived.length === 0 && (
                <div className="text-xs text-gray-400 dark:text-gray-500 text-center py-3 px-3">No archived chats</div>
              )}
              {archived.map(s => (
                <ChatRow key={s.id} s={s} activeChatId={activeChatId} onSelect={onSelect} onDelete={onDelete} onUnarchive={onUnarchive} archived />
              ))}
              {hasMoreArchivedChats && (
                <button
                  onClick={onLoadMoreArchivedChats}
                  disabled={loadingMoreArchivedChats}
                  aria-label="Load more archived chats"
                  className="w-full min-h-[44px] text-xs text-center text-blue-600 dark:text-blue-400 hover:bg-gray-50 dark:hover:bg-gray-700 disabled:opacity-50 transition-colors"
                >
                  {loadingMoreArchivedChats ? 'Loading…' : 'Load more'}
                </button>
              )}
            </>
          )}
        </div>

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
