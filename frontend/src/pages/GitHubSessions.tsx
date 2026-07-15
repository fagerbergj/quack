import { useEffect, useState } from 'react'
import { api, type ChatSummary } from '../api'
import { navigate } from '../router'

export interface GithubRepoGroup {
  repo: string
  chats: ChatSummary[]
}

// isGithubChat: github_url is the authoritative signal (set by the webhook at
// dispatch time); the id prefix is a fallback for chats persisted before that
// field existed.
export function isGithubChat(c: ChatSummary): boolean {
  return Boolean(c.github_url) || c.id.startsWith('github-')
}

// groupGithubChats filters to GitHub-originated chats, groups by repo (falling
// back to the chat id when github_repo is missing), and sorts groups and their
// chats by updated_at descending.
export function groupGithubChats(chats: ChatSummary[]): GithubRepoGroup[] {
  const byRepo = new Map<string, ChatSummary[]>()
  for (const c of chats) {
    if (!isGithubChat(c)) continue
    const repo = c.github_repo ?? c.id
    const bucket = byRepo.get(repo)
    if (bucket) bucket.push(c)
    else byRepo.set(repo, [c])
  }
  const groups = Array.from(byRepo, ([repo, groupChats]) => ({
    repo,
    chats: groupChats.sort((a, b) => b.updated_at.localeCompare(a.updated_at)),
  }))
  groups.sort((a, b) => (b.chats[0]?.updated_at ?? '').localeCompare(a.chats[0]?.updated_at ?? ''))
  return groups
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

const STATUS_STYLES: Record<ChatSummary['status'], string> = {
  running: 'bg-blue-100 text-blue-600 dark:bg-blue-900/40 dark:text-blue-400',
  needs_input: 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-400',
  failed: 'bg-red-100 text-red-600 dark:bg-red-900/40 dark:text-red-400',
  idle: 'bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400',
}

function StatusPill({ status }: { status: ChatSummary['status'] }) {
  return (
    <span className={`text-[10px] font-medium px-1.5 py-0.5 rounded ${STATUS_STYLES[status]}`}>
      {status.replace('_', ' ')}
    </span>
  )
}

// GitHubSessionsView is the presentational split — it takes already-fetched chats
// so it can be storied/tested with injected data (Storybook has no MSW to mock the
// fetch the default export does). The default export below owns data loading.
export function GitHubSessionsView({ chats }: { chats: ChatSummary[] }) {
  const groups = groupGithubChats(chats)

  if (groups.length === 0) {
    return (
      <div className="h-full flex items-center justify-center text-sm text-gray-400 dark:text-gray-500">
        No GitHub-originated sessions yet.
      </div>
    )
  }

  return (
    <div className="h-full overflow-y-auto overscroll-contain bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-white">
      <div className="max-w-3xl mx-auto py-6 px-4 space-y-6">
        {groups.map(group => (
          <div key={group.repo}>
            <h2 className="text-xs font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500 mb-2">
              {group.repo}
            </h2>
            <div className="rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 divide-y divide-gray-100 dark:divide-gray-700">
              {group.chats.map(c => (
                <div
                  key={c.id}
                  onClick={() => navigate('/chat/' + c.id)}
                  className="flex items-center justify-between gap-3 px-3 py-2.5 cursor-pointer hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors"
                >
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <span title={c.title || c.id} className="text-sm truncate text-gray-800 dark:text-gray-100">
                        {c.title || c.id}
                      </span>
                      <StatusPill status={c.status} />
                    </div>
                    <span className="text-xs text-gray-400 dark:text-gray-500">{relativeDate(c.updated_at)}</span>
                  </div>
                  {c.github_url && (
                    <a
                      href={c.github_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      onClick={e => e.stopPropagation()}
                      aria-label={`Open ${c.github_url} on GitHub`}
                      className="flex-shrink-0 text-xs text-blue-600 dark:text-blue-400 hover:underline"
                    >
                      ↗
                    </a>
                  )}
                </div>
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

export default function GitHubSessions() {
  const [chats, setChats] = useState<ChatSummary[] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    api
      .listChats()
      .then(list => {
        if (!cancelled) setChats(list.data)
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      })
    return () => {
      cancelled = true
    }
  }, [])

  if (error) {
    return <div className="p-6 text-sm text-red-600 dark:text-red-400">Failed to load: {error}</div>
  }
  if (chats === null) {
    return <div className="p-6 text-sm text-gray-400 dark:text-gray-500">Loading…</div>
  }
  return <GitHubSessionsView chats={chats} />
}
