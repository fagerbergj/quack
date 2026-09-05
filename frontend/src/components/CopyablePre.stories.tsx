import type { Meta, StoryObj } from '@storybook/react-vite'
import { CopyablePre } from './CopyablePre'

const meta: Meta<typeof CopyablePre> = {
  title: 'Chat/CopyablePre',
  component: CopyablePre,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof CopyablePre>

const code = `func greet(name string) string {
\treturn "Hello, " + name
}`

// The copy button is opacity-0 until hover/focus in real use; shown here via
// the wrapping group so it's visible without an interaction.
export const Default: Story = {
  render: () => (
    <div className="group max-w-xl">
      <CopyablePre><code className="language-go">{code}</code></CopyablePre>
    </div>
  ),
}

export const Dark: Story = {
  ...Default,
  globals: { theme: 'dark' },
}

// 390x844: the copy button must stay reachable without covering wrapped code.
export const MobileViewport390: Story = {
  render: () => (
    <div className="w-[390px] h-[844px] overflow-hidden border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-900 p-3 group">
      <CopyablePre><code className="language-go">{code}</code></CopyablePre>
    </div>
  ),
}
