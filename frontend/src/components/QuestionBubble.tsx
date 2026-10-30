import { useState } from 'react'
import { agentLabel } from './messageParts'
import { Icon } from './Icon'
import { Button } from './ui/button'
import { Card } from './ui/card'
import { Textarea } from './ui/input'

// Serves both get_user_choice clarifications and a paused node's HITL question. The answer goes out as
// the next chat message, which the backend resumes as the tool response or the node's input.
export function QuestionBubble({
  agent,
  question,
  options = [],
  disabled,
  answered,
  onSelect,
}: {
  agent: string
  question?: string
  options?: string[]
  disabled?: boolean
  answered?: string    // when set, render a read-only resolved view (no inputs)
  onSelect: (answer: string) => void
}) {
  const [freeform, setFreeform] = useState('')

  function submitFreeform(e: React.FormEvent) {
    e.preventDefault()
    const trimmed = freeform.trim()
    if (!trimmed || disabled) return
    setFreeform('')
    onSelect(trimmed)
  }

  // Resolved: the user has answered. Show the question + chosen answer, no inputs.
  if (answered != null) {
    return (
      <Card className="mt-3 border-l-4 px-4 py-3">
        <div className="flex items-center gap-2 text-xs font-medium text-green-700 dark:text-green-400">
          <Icon name="check" className="w-3.5 h-3.5" />
          Answered · {agentLabel(agent)}
        </div>
        {question && (
          <p className="mt-1 text-sm text-gray-500 dark:text-gray-400">{question}</p>
        )}
        <p className="mt-0.5 text-sm text-gray-800 dark:text-gray-100">{answered}</p>
      </Card>
    )
  }

  return (
    <Card
      className="mt-3 border-l-4 border-primary px-4 py-3"
      role="group"
      aria-label={`${agentLabel(agent)} needs your input`}
    >
      <div className="flex items-center gap-2 text-xs font-medium text-blue-700 dark:text-blue-300">
        <Icon name="help" className="w-3.5 h-3.5" />
        {agentLabel(agent)} needs your input
      </div>
      {question && (
        <p className="mt-1 text-sm text-gray-800 dark:text-gray-100">{question}</p>
      )}
      <p className="mt-0.5 text-xs text-blue-600/80 dark:text-blue-400/80">
        {options.length > 0 ? 'Pick an option, or type your own answer.' : 'Type your answer below.'}
      </p>
      {options.length > 0 && (
        <div className="mt-3 flex flex-wrap gap-2">
          {options.map((opt, i) => (
            <Button
              key={i}
              variant="outline"
              disabled={disabled}
              onClick={() => onSelect(opt)}
              className="h-auto min-h-11 max-w-full justify-start whitespace-normal break-words py-2 text-left"
            >
              {opt}
            </Button>
          ))}
        </div>
      )}
      <form onSubmit={submitFreeform} className="mt-3 flex gap-2 items-end">
        <Textarea
          value={freeform}
          rows={2}
          disabled={disabled}
          onChange={e => setFreeform(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter' && !e.shiftKey) submitFreeform(e) }}
          placeholder="Type your answer… (Enter to send, Shift+Enter for newline)"
          aria-label="Type your answer"
          className="flex-1 resize-none"
        />
        <Button type="submit" disabled={disabled || !freeform.trim()}>Send</Button>
      </form>
    </Card>
  )
}
