import { useState } from 'react'
import { agentLabel } from './messageParts'

// QuestionBubble is the single, attributed question-prompt UI shared by every
// place the app needs a human answer mid-conversation:
//  - the orchestrator's get_user_choice clarification (options + freeform)
//  - a paused node's mid-node HITL question (freeform only — options is empty)
// Both render identically at the conversation level, credited to whoever asked
// (agent). Clicking an option or submitting the box calls onSelect with that
// text; the caller sends it as the next chat message, which the backend resumes
// as the answer (get_user_choice's tool response, or the paused node's input).
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
      <div className="mt-3 rounded-xl border border-gray-200 dark:border-gray-700 border-l-4 bg-gray-50 dark:bg-gray-800/40 px-4 py-3">
        <div className="flex items-center gap-1.5 text-xs font-medium text-green-700 dark:text-green-400">
          <span aria-hidden="true">✓</span>
          Answered · {agentLabel(agent)}
        </div>
        {question && (
          <p className="mt-1 text-sm text-gray-500 dark:text-gray-400">{question}</p>
        )}
        <p className="mt-0.5 text-sm text-gray-800 dark:text-gray-100">{answered}</p>
      </div>
    )
  }

  return (
    <div
      className="mt-3 rounded-xl border border-blue-300 dark:border-blue-700 border-l-4 bg-blue-50/60 dark:bg-blue-950/30 px-4 py-3"
      role="group"
      aria-label={`${agentLabel(agent)} needs your input`}
    >
      <div className="flex items-center gap-1.5 text-xs font-medium text-blue-700 dark:text-blue-300">
        <span aria-hidden="true">❓</span>
        {agentLabel(agent)} needs your input
      </div>
      {question && (
        <p className="mt-1 text-sm text-gray-800 dark:text-gray-100">{question}</p>
      )}
      <p className="mt-0.5 text-xs text-blue-600/80 dark:text-blue-400/80">
        {options.length > 0 ? 'Pick an option, or type your own answer.' : 'Type your answer below.'}
      </p>
      {options.length > 0 && (
        <div className="mt-2.5 flex flex-wrap gap-2">
          {options.map((opt, i) => (
            <button
              key={i}
              type="button"
              disabled={disabled}
              onClick={() => onSelect(opt)}
              className="max-w-full px-3 py-2 rounded-xl border border-blue-300 dark:border-blue-700 bg-white dark:bg-gray-800 text-sm text-left text-blue-700 dark:text-blue-300 whitespace-normal break-words hover:bg-blue-100 dark:hover:bg-blue-950/60 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
            >
              {opt}
            </button>
          ))}
        </div>
      )}
      <form onSubmit={submitFreeform} className="mt-2.5 flex gap-2 items-end">
        <textarea
          value={freeform}
          rows={2}
          disabled={disabled}
          onChange={e => setFreeform(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter' && !e.shiftKey) submitFreeform(e) }}
          placeholder="Type your answer… (Enter to send, Shift+Enter for newline)"
          aria-label="Type your answer"
          className="flex-1 rounded-xl border border-blue-300 dark:border-blue-700 bg-white dark:bg-gray-800 px-3 py-2 text-sm resize-none focus:outline-none focus:ring-2 focus:ring-blue-500 disabled:opacity-50 dark:text-gray-100 dark:placeholder-gray-400"
        />
        <button
          type="submit"
          disabled={disabled || !freeform.trim()}
          className="px-3 py-2 rounded-xl bg-blue-600 text-white text-sm font-medium hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
        >
          Send
        </button>
      </form>
    </div>
  )
}
