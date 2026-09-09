import type { Meta, StoryObj } from '@storybook/react-vite'
import { CopyButton } from './CopyButton'

const meta: Meta<typeof CopyButton> = {
  title: 'Chat/CopyButton',
  component: CopyButton,
  parameters: { layout: 'centered' },
}
export default meta

type Story = StoryObj<typeof CopyButton>

// Click it - the glyph flashes ✓ for 2s to confirm the copy (clipboard writes
// are inert in a sandboxed Storybook iframe, but the visual feedback still fires).
export const Default: Story = {
  args: { text: '{\n  "input": { "path": "a.go" },\n  "output": { "replacements": 1 }\n}' },
}

export const CustomLabel: Story = {
  args: { text: 'go test ./...', label: 'Copy command' },
}

// Compact width: the glyph stays 12px but the hit area grows to 44px (it sits
// beside 44px-tall Raw/Diff buttons in the artifacts sheet); desktop keeps
// the bare-glyph footprint.
export const MobileViewport: Story = {
  args: { text: 'go test ./...' },
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'] } },
}
