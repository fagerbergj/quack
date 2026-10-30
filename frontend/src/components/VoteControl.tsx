import { useState } from 'react'
import type { VoteDirection } from '../api'
import { Icon } from './Icon'
import { Button } from './ui/button'
import { Tooltip } from './ui/tooltip'

export interface VoteControlProps {
  score: number
  ownVote?: 'up' | 'down'
  onVote: (vote: VoteDirection) => Promise<void>
  disabled?: boolean
}

// Clicking the active arrow sends "none". If onVote rejects, local pending state rolls back to the
// pre-click arrow so a failed request never leaves the highlight stuck wrong.
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

  const activeClass = 'text-primary dark:text-blue-400'

  return (
    // Stacked on compact width so the row-end rail (#1266) stays 88px wide with the kebab, leaving the text room.
    <div className="inline-flex flex-col medium:flex-row items-center gap-0.5" role="group" aria-label="Vote on this memory">
      <Tooltip content="This memory helped">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => { void handleClick('up') }}
          disabled={pending || disabled}
          aria-pressed={ownVote === 'up'}
          aria-label="Upvote"
          className={`size-11 medium:size-11 ${ownVote === 'up' ? activeClass : ''}`}
        >
          <Icon name="arrow_upward" className="w-3.5 h-3.5" />
        </Button>
      </Tooltip>
      <span className="min-w-[1.5em] text-center text-xs font-medium tabular-nums text-foreground">{score}</span>
      <Tooltip content="This memory was wrong">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => { void handleClick('down') }}
          disabled={pending || disabled}
          aria-pressed={ownVote === 'down'}
          aria-label="Downvote"
          className={`size-11 medium:size-11 ${ownVote === 'down' ? activeClass : ''}`}
        >
          <Icon name="arrow_downward" className="w-3.5 h-3.5" />
        </Button>
      </Tooltip>
      {error && <span role="status" className="text-2xs text-red-700 dark:text-red-400 ml-1">Vote failed</span>}
    </div>
  )
}
