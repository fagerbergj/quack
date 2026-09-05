import type { Meta, StoryObj } from '@storybook/react-vite'
import { MermaidDiagram } from './MermaidDiagram'

const meta: Meta<typeof MermaidDiagram> = {
  title: 'Chat/MermaidDiagram',
  component: MermaidDiagram,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof MermaidDiagram>

const diagram = `graph TD
  A[Start] --> B{Decision}
  B -->|Yes| C[Do it]
  B -->|No| D[Skip it]`

// mermaid loads async on mount (see loadMermaid) - this renders the real
// diagram in Storybook, no network involved (the package is bundled).
export const Default: Story = {
  args: { code: diagram },
}

export const Dark: Story = {
  args: { code: diagram },
  globals: { theme: 'dark' },
}

// Invalid mermaid source falls back to CopyablePre + a warning notice.
export const InvalidSource: Story = {
  args: { code: 'not a valid diagram {{{' },
}

export const MobileViewport390: Story = {
  args: { code: diagram },
  // fullscreen: this frame IS the simulated device width - the preview's own
  // docs-canvas padding would otherwise push it past 390px (render-check).
  parameters: { layout: 'fullscreen' },
  decorators: [Story => (
    <div className="w-[390px] h-[844px] overflow-hidden border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-900 p-3">
      <Story />
    </div>
  )],
}
