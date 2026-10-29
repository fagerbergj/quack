import { Component, type ReactNode } from 'react'
import { Icon } from './Icon'
import { Button } from './ui/button'

interface Props { children: ReactNode; fallback?: ReactNode }
interface State { failed: boolean }

// Keeps a failed lazy chunk or a render crash inside its box instead of white-screening the app.
// fallback replaces the default full-page reload prompt. Class-only: hooks can't catch render errors.
export class LazyLoadBoundary extends Component<Props, State> {
  state: State = { failed: false }

  static getDerivedStateFromError() {
    return { failed: true }
  }

  componentDidCatch(error: unknown) {
    console.error('LazyLoadBoundary: subtree failed to load or render', error)
  }

  render() {
    if (this.state.failed) {
      if (this.props.fallback !== undefined) return this.props.fallback
      return (
        <div className="flex-1 flex flex-col items-center justify-center gap-3 p-6 text-center text-sm text-gray-500 dark:text-gray-400">
          <p>This page couldn&apos;t load. Check your connection and try again.</p>
          <Button onClick={() => window.location.reload()}>
            <Icon name="refresh" className="w-4 h-4" />
            Reload
          </Button>
        </div>
      )
    }
    return this.props.children
  }
}
