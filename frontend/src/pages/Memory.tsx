import { MemoryTab } from '../components/MemoryTab'
import { NavToggle } from '../components/NavToggle'

// App.tsx owns the drawer state so the header toggle and the NavRail overlay share one source of truth.
export interface MemoryProps {
  navOpen: boolean
  onToggleNav: () => void
}

export default function Memory({ navOpen, onToggleNav }: MemoryProps) {
  return (
    <div className="flex flex-col h-full bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-white">
      <div className="flex items-center gap-3 px-4 py-3 sm:px-6 border-b border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800">
        <NavToggle open={navOpen} onToggle={onToggleNav} />
        <h1 className="text-base font-semibold text-gray-900 dark:text-white">Memory</h1>
      </div>
      <div className="flex-1 min-h-0">
        <MemoryTab />
      </div>
    </div>
  )
}
