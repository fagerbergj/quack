import type { ChatSummary } from '../api'

// isGithubChat: github_url is the authoritative signal (set by the webhook at
// dispatch time); the id prefix is a fallback for chats persisted before that
// field existed.
export function isGithubChat(c: ChatSummary): boolean {
  return Boolean(c.github_url) || c.id.startsWith('github-')
}

export interface GithubRef {
  repo: string
  kind: 'issue' | 'pr'
  number: number
}

const GITHUB_URL_RE = /\/(issues|pull)\/(\d+)/

// parseGithubRef extracts the {repo, kind, number} the row's Issue/PR badge and
// the Repo/Type facets need, straight off github_url - the same field
// isGithubChat trusts. Undefined for a non-GitHub chat or an unrecognized URL shape.
export function parseGithubRef(c: ChatSummary): GithubRef | undefined {
  if (!c.github_url) return undefined
  const m = c.github_url.match(GITHUB_URL_RE)
  if (!m) return undefined
  const repo = c.github_repo ?? c.github_url.replace(/^https?:\/\/github\.com\//, '').split('/').slice(0, 2).join('/')
  return { repo, kind: m[1] === 'pull' ? 'pr' : 'issue', number: Number(m[2]) }
}
