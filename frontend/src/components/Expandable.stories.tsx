import type { Meta, StoryObj } from '@storybook/react-vite'
import { Expandable } from './Expandable'

const meta: Meta<typeof Expandable> = {
  title: 'Chat/Expandable',
  component: Expandable,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof Expandable>

const short = 'A single short paragraph that comfortably fits inside the cap, so no toggle should appear.'
const long = Array.from({ length: 40 }, (_, i) => `Line ${i + 1}: the quick brown fox jumps over the lazy dog.`).join('\n')

// Content shorter than the cap: renders as-is, no fade, no toggle.
export const Fits: Story = {
  render: () => (
    <Expandable maxHeight={240}>
      <p className="text-sm text-gray-700 dark:text-gray-200">{short}</p>
    </Expandable>
  ),
}

// Content taller than the cap: clamps to maxHeight with a fade + Show more toggle.
export const Overflows: Story = {
  render: () => (
    <Expandable maxHeight={160}>
      <pre className="whitespace-pre-wrap font-mono text-xs text-gray-700 dark:text-gray-200">{long}</pre>
    </Expandable>
  ),
}

// A tiny cap over a code surface: the fade is matched to the code background.
export const OnCodeSurface: Story = {
  render: () => (
    <Expandable maxHeight={100} fade="from-gray-50 dark:from-gray-900">
      <pre className="bg-gray-50 dark:bg-gray-900 rounded p-2 whitespace-pre-wrap font-mono text-xs text-gray-700 dark:text-gray-200">{long}</pre>
    </Expandable>
  ),
}
