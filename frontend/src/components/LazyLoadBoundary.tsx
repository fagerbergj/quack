import { Component, type ReactNode } from 'react'
import { Icon } from './Icon'

interface Props { children: ReactNode }
interface State { failed: boolean }

// Wraps a React.lazy route (Memory/ExtensionHost in App.tsx): a failed chunk
// fetch (deploy rolled the asset hashes mid-session, tab went offline) otherwise
// throws past Suspense and white-screens the whole app. Must be a class component - React has no hook equivalent for getDerivedStateFromError/componentDidCatch.
export class LazyLoadBoundary extends Component<Props, State> {
  state: State = { failed: false }

  static getDerivedStateFromError() {
    return { failed: true }
  }

  componentDidCatch(error: unknown) {
    console.error('LazyLoadBoundary: route chunk failed to load', error)
  }

  render() {
    if (this.state.failed) {
      return (
        <div className="flex-1 flex flex-col items-center justify-center gap-3 p-6 text-center text-sm text-gray-500 dark:text-gray-400">
          <p>This page couldn&apos;t load. Check your connection and try again.</p>
          <button
            type="button"
            onClick={() => window.location.reload()}
            className="flex items-center gap-1.5 rounded-xl bg-blue-600 px-4 py-2 text-sm text-white hover:bg-blue-700 transition-colors"
          >
            <Icon name="refresh" className="w-4 h-4" />
            Reload
          </button>
        </div>
      )
    }
    return this.props.children
  }
}
