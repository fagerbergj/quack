import { useState } from 'react'
import type { VoteDirection } from '../api'
import { Icon } from './Icon'

export interface VoteControlProps {
  score: number
  ownVote?: 'up' | 'down'
  onVote: (vote: VoteDirection) => Promise<void>
  disabled?: boolean
}

// VoteControl: a Reddit-style up arrow / net score / down arrow (epic #1255
// P4), shared by the memory page and the chat's per-node memories block.
// Clicking the currently-active arrow again sends "none" (toggle off);
// clicking the other arrow switches. The visible score updates optimistically
// (caller passes the already-mutated `score`/`ownVote` on success) and this
// component rolls its own local pending state back to the pre-click arrow if
// onVote rejects, so a failed request never leaves the highlight stuck wrong.
export function VoteControl({ score, ownVote, onVote, disabled }: VoteControlProps) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState(false)

  async function handleClick(direction: 'up' | 'down') {
    if (pending || disabled) return
    const next: VoteDirection = ownVote === direction ? 'none' : direction
    setPending(true)
    setError(false)
    try {
      await onVote(next)
    } catch {
      setError(true)
    } finally {
      setPending(false)
    }
  }

  const activeClass = 'text-blue-600 dark:text-blue-400'
  const idleClass = 'text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300'

  return (
    <div className="inline-flex items-center gap-0.5" role="group" aria-label="Vote on this memory">
      <button
        type="button"
        onClick={() => handleClick('up')}
        disabled={pending || disabled}
        aria-pressed={ownVote === 'up'}
        aria-label="Upvote"
        title="This memory helped"
        className={`min-w-[44px] min-h-[44px] flex items-center justify-center rounded transition-colors disabled:opacity-50 ${ownVote === 'up' ? activeClass : idleClass}`}
      >
        <Icon name="arrow_upward" className="w-3.5 h-3.5" />
      </button>
      <span className="min-w-[1.5em] text-center text-xs font-medium tabular-nums text-gray-600 dark:text-gray-300">{score}</span>
      <button
        type="button"
        onClick={() => handleClick('down')}
        disabled={pending || disabled}
        aria-pressed={ownVote === 'down'}
        aria-label="Downvote"
        title="This memory was wrong"
        className={`min-w-[44px] min-h-[44px] flex items-center justify-center rounded transition-colors disabled:opacity-50 ${ownVote === 'down' ? activeClass : idleClass}`}
      >
        <Icon name="arrow_downward" className="w-3.5 h-3.5" />
      </button>
      {error && <span role="status" className="text-[11px] text-red-500 dark:text-red-400 ml-1">Vote failed</span>}
    </div>
  )
}
