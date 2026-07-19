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
      className={`inline-flex items-center gap-1 text-xs text-blue-600 dark:text-blue-400 hover:underline ${className}`}
    >
      {repo && <span className="truncate">{repo}</span>}
      <span aria-hidden="true">↗</span>
    </a>
  )
}
