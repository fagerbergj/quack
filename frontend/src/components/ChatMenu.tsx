import { UsageSummary, type UsageSummaryProps } from './UsageSummary'
import { useTheme, type Theme } from '../hooks/useTheme'
import { Icon } from './Icon'
import { Button } from './ui/button'
import { Tooltip } from './ui/tooltip'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuSeparator, DropdownMenuTrigger } from './ui/dropdown-menu'

const THEME_OPTIONS: { value: Theme; label: string }[] = [
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
  { value: 'system', label: 'System' },
]

// Per-chat actions only; Memory is a NavRail peer of Chats, not a per-chat action, so it does not belong here.
export function ChatMenu({ chatId, usage }: { chatId: string; usage?: UsageSummaryProps }) {
  const [theme, setTheme] = useTheme()

  return (
    <div className="relative flex-shrink-0">
      <DropdownMenu>
        <Tooltip content="Chat actions">
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" aria-label="Chat actions" className="size-11 medium:size-11">
              <Icon name="more_horiz" className="w-5 h-5" />
            </Button>
          </DropdownMenuTrigger>
        </Tooltip>
        <DropdownMenuContent className="medium:w-44">
          {/* Shown only where the header's `hidden medium:flex` UsageSummary (Chat.tsx) is hidden. */}
          {usage && (usage.models.length > 0 || (usage.usage?.total_tokens ?? 0) > 0) && (
            <>
              <div className="px-3 py-2 medium:hidden">
                <UsageSummary {...usage} />
              </div>
              <DropdownMenuSeparator className="medium:hidden" />
            </>
          )}
          <DropdownMenuItem asChild>
            <a
              href={`/api/v1/chats/${chatId}/recording`}
              title="Download this chat's full recording (every streamed event)"
            >
              <Icon name="download" className="w-3.5 h-3.5" /> Download Logs
            </a>
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          {/* APG menuitemradio: selecting leaves the menu open so the user can change their mind without reopening. */}
          <DropdownMenuRadioGroup aria-label="Theme" value={theme} onValueChange={v => setTheme(v as Theme)}>
            {THEME_OPTIONS.map(opt => (
              <DropdownMenuRadioItem key={opt.value} value={opt.value} onSelect={e => e.preventDefault()}>
                {opt.label}
              </DropdownMenuRadioItem>
            ))}
          </DropdownMenuRadioGroup>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}
