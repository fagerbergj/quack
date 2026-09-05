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

// CopyablePre's own wrapper already carries `group` (opacity-0
// group-hover:opacity-100) - the button stays hover/focus-gated here too,
// same as real use. Hover the code block (or tab to the button) to see it.
export const Default: Story = {
  render: () => (
    <div className="max-w-xl">
      <CopyablePre><code className="language-go">{code}</code></CopyablePre>
    </div>
  ),
}

export const Dark: Story = {
  ...Default,
  globals: { theme: 'dark' },
}

// 390x844: the copy button must stay reachable without covering wrapped code.
// fullscreen: this frame IS the simulated device width - the preview's own
// docs-canvas padding would otherwise push it past 390px (render-check).
export const MobileViewport390: Story = {
  parameters: { layout: 'fullscreen' },
  render: () => (
    <div className="w-[390px] h-[844px] overflow-hidden border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-900 p-3">
      <CopyablePre><code className="language-go">{code}</code></CopyablePre>
    </div>
  ),
}
