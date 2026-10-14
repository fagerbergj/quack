import { Icon } from './Icon'

// A dropped live stream (usually a deploy) while the store retries; sits above the composer so it is never
// scrolled under it. An error banner supersedes it.
export function ReconnectingBanner({ reconnecting, error }: { reconnecting: boolean; error?: string }) {
  if (!reconnecting || error) return null
  return (
    <div role="status" className="mx-4 mb-2 flex items-center gap-2 rounded-md border border-amber-200 dark:border-amber-800 bg-amber-50 dark:bg-amber-950/30 px-4 py-2 text-sm text-amber-800 dark:text-amber-300">
      <Icon name="warning" className="w-4 h-4 shrink-0" />
      Connection lost - reconnecting. The run keeps going on the server.
    </div>
  )
}
