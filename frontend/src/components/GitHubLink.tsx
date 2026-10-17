// GitHubLink renders a deep link back to the GitHub issue/PR that originated a
// chat. Used by the chat header (Chat.tsx).
export function GitHubLink({ url, repo, className = '' }: { url: string; repo?: string; className?: string }) {
  return (
    <a
      href={url}
      target="_blank"
      rel="noopener noreferrer"
      onClick={e => e.stopPropagation()}
      aria-label={repo ? `Open ${repo} on GitHub` : 'Open on GitHub'}
      className={`inline-flex items-center justify-center min-h-[24px] min-w-[24px] gap-1 text-xs text-blue-600 dark:text-blue-400 hover:underline ${className}`}
    >
      {/* Compact width keeps the arrow only, so the chat title gets the room; the aria-label still names the repo. */}
      {repo && <span className="hidden medium:inline truncate">{repo}</span>}
      <span aria-hidden="true">↗</span>
    </a>
  )
}
