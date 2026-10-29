import { Icon } from './Icon'
import { Button } from './ui/button'
import { Tooltip } from './ui/tooltip'

// The single drawer trigger, in each page header. Its grid glyph must never match the chat-list toggle's
// menu icon, so the app never shows two identical hamburgers.
export interface NavToggleProps {
  open: boolean
  onToggle: () => void
}

export function NavToggle({ open, onToggle }: NavToggleProps) {
  return (
    <Tooltip content="Toggle navigation">
      <Button
        variant="ghost"
        size="icon"
        onClick={onToggle}
        aria-label="Toggle navigation"
        aria-haspopup="dialog"
        aria-expanded={open}
        className="size-11 medium:size-11"
      >
        <Icon name="menu_grid" className="w-5 h-5" />
      </Button>
    </Tooltip>
  )
}
