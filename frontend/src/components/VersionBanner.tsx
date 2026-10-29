import type { Skew } from '../state/clientConfig'
import { Icon } from './Icon'
import { Button } from './ui/button'
import { Dialog, DialogContent } from './ui/dialog'

function ReloadButton({ autoFocus }: { autoFocus?: boolean }) {
  return (
    <Button data-autofocus={autoFocus || undefined} onClick={() => window.location.reload()}>
      <Icon name="refresh" className="w-4 h-4" />
      Reload
    </Button>
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
  return <Blocked />
}

// A controlled Dialog with no onOpenChange: Esc and outside clicks cannot dismiss it. Its blocking placement sits
// at z-70, above every other Radix layer, and the modal makes the app behind it inert.
function Blocked() {
  return (
    <Dialog open>
      <DialogContent title="Reload required" placement="blocking" role="alertdialog">
        <p>quack was updated and this page no longer works with it - reload to get the new version.</p>
        <ReloadButton autoFocus />
      </DialogContent>
    </Dialog>
  )
}
