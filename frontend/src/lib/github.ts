import type { ChatSummary } from '../api'

// github_url is the authoritative signal (set by the webhook); the id prefix covers chats persisted before it existed.
export function isGithubChat(c: ChatSummary): boolean {
  return Boolean(c.github_url) || c.id.startsWith('github-')
}

export interface GithubRef {
  repo: string
  kind: 'issue' | 'pr'
  number: number
}

const GITHUB_URL_RE = /\/(issues|pull)\/(\d+)/

// The {repo, kind, number} for the Issue/PR badge and Repo/Type facets, read off github_url like isGithubChat.
// Undefined for a non-GitHub chat or an unrecognized URL shape.
export function parseGithubRef(c: ChatSummary): GithubRef | undefined {
  if (!c.github_url) return undefined
  const m = c.github_url.match(GITHUB_URL_RE)
  if (!m) return undefined
  const repo = c.github_repo ?? c.github_url.replace(/^https?:\/\/github\.com\//, '').split('/').slice(0, 2).join('/')
  return { repo, kind: m[1] === 'pull' ? 'pr' : 'issue', number: Number(m[2]) }
}
