import { MemoryTab } from '../components/MemoryTab'

// The app's second page (#727): quack's semantic memory, browsable and
// forgettable. Routed by router.ts's plain path-prefix matcher - see App.tsx.
// Reached from the NavRail now (#746 item 1), not a page-local back-link.
export default function Memory() {
  return (
    <div className="flex flex-col h-full bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-white">
      <div className="flex items-center gap-3 px-4 py-3 sm:px-6 border-b border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800">
        <h1 className="text-base font-semibold text-gray-900 dark:text-white">Memory</h1>
      </div>
      <div className="flex-1 min-h-0">
        <MemoryTab />
      </div>
    </div>
  )
}
