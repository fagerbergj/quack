import { useMemo, useState } from 'react'
import type { ChatSummary } from '../api'
import { isGithubChat, parseGithubRef, type GithubRef } from '../lib/github'
import { computeFacets, filterChats, parseFilterState, serializeFilterState, type SelectedFacets } from '../lib/chatFilters'
import { paletteTone } from '../lib/colorHash'
import { Badge, type BadgeTone } from './ui/badge'
import { FilterPanel } from './FilterPanel'
import { StatusDot } from './StatusDot'
import { navigate, useSearch } from '../router'
import { useMediaQuery } from '../hooks/useMediaQuery'
import { Button } from './ui/button'
import { Input } from './ui/input'
import { Sheet, SheetContent } from './ui/sheet'
import { Tooltip } from './ui/tooltip'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from './ui/dropdown-menu'
import { Icon, type IconName } from './Icon'

// A chat with no title yet; the CLI prints the same string.
export const UNTITLED_CHAT = '(untitled)'

const GITHUB_STATE_TONE = new Map<string, BadgeTone>([['open', 'green'], ['closed', 'red'], ['merged', 'purple'], ['draft', 'yellow']])

export function githubStateTone(state: string): BadgeTone {
  return GITHUB_STATE_TONE.get(state) ?? 'gray'
}

// GitHub's colours for an origin badge, but never for 'draft': an extension's own badge
// vocabulary (a doc's "draft" revision label) must not be guessed at, so it stays neutral.
export function originBadgeTone(badge: string): BadgeTone {
  return (badge !== 'draft' && GITHUB_STATE_TONE.get(badge)) || 'gray'
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
  onDelete: (id: string) => void
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
        <Badge asChild tone={paletteTone(ref.repo)} className="max-w-[7rem] truncate hover:underline">
          <a
            href={`https://github.com/${ref.repo}`}
            target="_blank"
            rel="noopener noreferrer"
            onClick={e => e.stopPropagation()}
            title={ref.repo}
          >
            {ref.repo.slice(ref.repo.indexOf('/') + 1)}
          </a>
        </Badge>
      )}
      {ref && s.github_url && (
        <Badge asChild className="hover:text-primary hover:underline">
          <a
            href={s.github_url}
            target="_blank"
            rel="noopener noreferrer"
            onClick={e => e.stopPropagation()}
            title={ref.kind === 'pr' ? `Pull request #${ref.number}` : `Issue #${ref.number}`}
          >
            {ref.kind === 'pr' ? 'PR' : 'Issue'} #{ref.number}
          </a>
        </Badge>
      )}
      {s.github_state && (
        <Badge tone={githubStateTone(s.github_state)} title={s.github_state}>
          {githubStateIcon(s.github_state) && <Icon name={githubStateIcon(s.github_state)!} className="w-2.5 h-2.5" />}
          {GITHUB_STATE_TONE.has(s.github_state) && s.github_state}
        </Badge>
      )}
      {/* Extension-dispatched chats. GitHub's origin repeats the repo/PR/state badges above, so it is skipped. */}
      {s.origin && !(isGithubChat(s) && ref) && (
        <>
          {s.origin.href ? (
            <Badge asChild tone={paletteTone(s.origin.extension)} className="max-w-[7rem] truncate hover:underline">
              <a
                href={s.origin.href}
                target="_blank"
                rel="noopener noreferrer"
                onClick={e => e.stopPropagation()}
                title={s.origin.label}
              >
                {s.origin.label}
              </a>
            </Badge>
          ) : (
            <Badge tone={paletteTone(s.origin.extension)} className="max-w-[7rem] truncate" title={s.origin.label}>
              {s.origin.label}
            </Badge>
          )}
          {s.origin.badge && (
            <Badge tone={originBadgeTone(s.origin.badge)} title={s.origin.badge}>{s.origin.badge}</Badge>
          )}
        </>
      )}
    </>
  )
}

// Absolutely positioned, not in flow, so it never grows the row's height. Always visible: touch has no hover.
// The wrapper stops clicks, including ones bubbling out of the portalled menu, from selecting the row.
function ChatRowMenu({ archived, onArchive, onUnarchive, onDelete }: {
  archived?: boolean
  onArchive?: () => void
  onUnarchive?: () => void
  onDelete: () => void
}) {
  return (
    <div className="absolute right-0 top-0" onClick={e => e.stopPropagation()}>
      <DropdownMenu>
        <Tooltip content="Chat actions">
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" aria-label="Chat actions" className="size-11 medium:size-11">
              <Icon name="more_vert" className="w-5 h-5" />
            </Button>
          </DropdownMenuTrigger>
        </Tooltip>
        <DropdownMenuContent>
          {!archived && (
            <DropdownMenuItem aria-label="Archive chat" onSelect={onArchive}>
              <Icon name="archive" className="w-3.5 h-3.5" /> Archive
            </DropdownMenuItem>
          )}
          {archived && onUnarchive && (
            <DropdownMenuItem aria-label="Unarchive chat" onSelect={onUnarchive}>
              <Icon name="history" className="w-3.5 h-3.5" /> Restore
            </DropdownMenuItem>
          )}
          {archived && (
            <DropdownMenuItem destructive aria-label="Delete chat permanently" onSelect={onDelete}>
              <Icon name="delete" className="w-3.5 h-3.5" /> Delete
            </DropdownMenuItem>
          )}
        </DropdownMenuContent>
      </DropdownMenu>
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
  onDelete: (id: string) => void
  archived?: boolean
  onArchive?: (chatId: string) => void
  onUnarchive?: (chatId: string) => void
}) {
  const ref = parseGithubRef(s)
  // Only the irreversible path needs a confirm.
  function handleDelete() {
    if (window.confirm(`Permanently delete "${s.title || UNTITLED_CHAT}"? This can't be undone.`)) {
      onDelete(s.id)
    }
  }

  return (
    <div
      className={`group relative flex flex-col px-3 py-3 cursor-pointer border-b border-gray-100 dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors ${activeChatId === s.id ? 'bg-blue-50 dark:bg-blue-900/30' : ''}`}
      onClick={() => onSelect(s.id)}
    >
      <span title={s.title || UNTITLED_CHAT} className="flex items-center pr-11">
        <StatusDot status={s.status} className="mr-2" variant="chat" />
        <span className={`text-sm truncate block ${activeChatId === s.id ? 'text-blue-700 dark:text-blue-400 font-medium' : 'text-gray-800 dark:text-gray-100'}`}>
          {s.title || UNTITLED_CHAT}
        </span>
      </span>
      {/* Always rendered so every row reserves the same height and stays aligned. */}
      <div className="flex items-center gap-1 h-4 mt-0.5 pr-6 min-w-0 overflow-hidden">
        <ChatBadges s={s} githubRef={ref} />
      </div>
      <span className="text-xs text-gray-500 dark:text-gray-400 mt-0.5">{relativeDate(s.updated_at)}</span>
      <ChatRowMenu
        archived={archived}
        onArchive={onArchive && (() => onArchive(s.id))}
        onUnarchive={onUnarchive && (() => onUnarchive(s.id))}
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
  onDelete: (id: string) => void
  onUnarchive?: (chatId: string) => void
  hasMore?: boolean
  loading?: boolean
  onLoadMore?: () => void
}) {
  return (
    <div>
      <Button
        variant="ghost"
        size="sm"
        onClick={onToggle}
        className="w-full justify-start gap-2 rounded-none border-t border-border px-3"
        aria-expanded={expanded}
      >
        <span aria-hidden="true" className={`transition-transform inline-block ${expanded ? 'rotate-90' : ''}`}>›</span>
        Archived{loaded ? ` (${archived.length})` : ''}
      </Button>
      {expanded && (
        <>
          {archived.length === 0 && (
            <div className="text-xs text-gray-500 dark:text-gray-400 text-center py-3 px-3">No archived chats</div>
          )}
          {archived.map(s => (
            <ChatRow key={s.id} s={s} activeChatId={activeChatId} onSelect={onSelect} onDelete={onDelete} onUnarchive={onUnarchive} archived />
          ))}
          {hasMore && (
            <Button
              variant="ghost"
              size="sm"
              onClick={onLoadMore}
              disabled={loading}
              aria-label="Load more archived chats"
              className="w-full rounded-none text-primary"
            >
              {loading ? 'Loading…' : 'Load more'}
            </Button>
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

  // Below `medium` (600px) the list is an off-canvas modal Sheet; above it, a static sidebar.
  const offCanvas = useMediaQuery('(max-width: 599px)')

  const body = (
    <>
      <div className="p-3 border-b border-gray-200 dark:border-gray-700 flex items-center gap-2">
        <Button onClick={onNewChat} className="flex-1">New Chat</Button>
        <Button variant="ghost" size="icon" onClick={onCloseMobile} className="medium:hidden" aria-label="Close chat list">
          <Icon name="close" className="w-4 h-4" />
        </Button>
      </div>
      <div className="p-2 border-b border-gray-200 dark:border-gray-700 flex items-center gap-2">
        <Input
          type="search"
          value={q}
          onChange={e => setFilterState({ q: e.target.value })}
          placeholder="Search chats…"
          aria-label="Search chats"
          className="flex-1"
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
          <Button
            variant="ghost"
            size="sm"
            onClick={onLoadMoreChats}
            disabled={loadingMoreChats}
            className="w-full rounded-none text-primary"
          >
            {loadingMoreChats ? 'Loading…' : 'Load more'}
          </Button>
        )}
      </div>
    </>
  )
  return offCanvas ? (
    <Sheet open={open} onOpenChange={o => { if (!o) onCloseMobile() }}>
      <SheetContent title="Chat list" className="w-[250px]">{body}</SheetContent>
    </Sheet>
  ) : (
    <div className="flex h-dvh w-[250px] flex-shrink-0 flex-col border-r border-border bg-card">{body}</div>
  )
}
