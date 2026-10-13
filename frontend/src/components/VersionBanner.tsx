import type { Skew } from '../state/clientConfig'
import { Icon } from './Icon'

function ReloadButton({ autoFocus }: { autoFocus?: boolean }) {
  return (
    <button
      type="button"
      autoFocus={autoFocus}
      onClick={() => window.location.reload()}
      className="flex shrink-0 items-center gap-1.5 rounded-xl bg-blue-600 px-4 py-2 min-h-[44px] medium:min-h-0 text-sm text-white hover:bg-blue-700 transition-colors"
    >
      <Icon name="refresh" className="w-4 h-4" />
      Reload
    </button>
  )
}

// 'reload' is a strip above the page that leaves it usable; 'older-server' explains failures a reload can't fix;
// 'blocked' covers the app, since calls from this build would fail against the server's API.
export function VersionBanner({ skew }: { skew: Skew }) {
  if (skew === 'ok') return null
  if (skew === 'older-server') {
    return (
      <div role="status" className="flex items-center gap-3 border-b border-amber-200 dark:border-amber-800 bg-amber-50 dark:bg-amber-900/30 px-4 py-2 text-sm text-amber-800 dark:text-amber-300">
        <Icon name="warning" className="w-4 h-4 shrink-0" />
        <span className="flex-1">This page is newer than the quack server - some features may fail until the server is updated.</span>
      </div>
    )
  }
  if (skew === 'reload') {
    return (
      <div role="status" className="flex items-center gap-3 border-b border-blue-200 dark:border-blue-800 bg-blue-50 dark:bg-blue-900/30 px-4 py-2 text-sm text-blue-800 dark:text-blue-300">
        <Icon name="refresh" className="w-4 h-4 shrink-0" />
        <span className="flex-1">quack was updated - reload to get the new version.</span>
        <ReloadButton />
      </div>
    )
  }
  return (
    <div role="alertdialog" aria-modal="true" aria-labelledby="version-blocked-msg" className="fixed inset-0 z-50 flex flex-col items-center justify-center gap-3 bg-white/95 dark:bg-gray-900/95 p-6 text-center text-sm text-gray-700 dark:text-gray-300">
      <p id="version-blocked-msg">quack was updated and this page no longer works with it - reload to get the new version.</p>
      <ReloadButton autoFocus />
    </div>
  )
}
