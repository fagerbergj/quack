// ChoicePrompt renders a get_user_choice clarification as a button group. Clicking
// an option calls onSelect with that option's text; the caller sends it as the next
// chat message, which the backend resumes as the tool's answer. The text box stays
// usable for free-form answers — these buttons are a one-click convenience.
export function ChoicePrompt({
  options,
  disabled,
  onSelect,
}: {
  options: string[]
  disabled?: boolean
  onSelect: (option: string) => void
}) {
  if (options.length === 0) return null
  return (
    <div className="mt-3 flex flex-wrap gap-2" role="group" aria-label="Choose an option">
      {options.map((opt, i) => (
        <button
          key={i}
          type="button"
          disabled={disabled}
          onClick={() => onSelect(opt)}
          className="px-3 py-2 rounded-xl border border-blue-300 dark:border-blue-700 text-sm text-blue-700 dark:text-blue-300 hover:bg-blue-50 dark:hover:bg-blue-950/40 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
        >
          {opt}
        </button>
      ))}
    </div>
  )
}
