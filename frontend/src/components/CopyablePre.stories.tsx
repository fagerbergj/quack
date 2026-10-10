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

// The button is hover/focus-gated as in real use: hover the code block or tab to the button to see it.
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

// The copy button must stay reachable without covering wrapped code. fullscreen keeps the docs-canvas
// padding from pushing the 390px frame wider.
export const MobileViewport390: Story = {
  parameters: { layout: 'fullscreen' },
  render: () => (
    <div className="w-[390px] h-[844px] overflow-hidden border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-900 p-3">
      <CopyablePre><code className="language-go">{code}</code></CopyablePre>
    </div>
  ),
}
