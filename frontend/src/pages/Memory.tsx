import { MemoryTab } from '../components/MemoryTab'
import { NavToggle } from '../components/NavToggle'

// App.tsx owns the drawer state so the header toggle and the NavRail overlay share one source of truth.
export interface MemoryProps {
  navOpen: boolean
  onToggleNav: () => void
}

export default function Memory({ navOpen, onToggleNav }: MemoryProps) {
  return (
    <div className="flex flex-col h-full bg-background text-foreground">
      <div className="flex items-center gap-3 px-4 py-3 sm:px-6 border-b border-border bg-card">
        <NavToggle open={navOpen} onToggle={onToggleNav} />
        <h1 className="text-base font-semibold text-gray-900 dark:text-white">Memory</h1>
      </div>
      <div className="flex-1 min-h-0">
        <MemoryTab />
      </div>
    </div>
  )
}
