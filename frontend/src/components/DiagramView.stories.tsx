import type { Meta, StoryObj } from '@storybook/react-vite'
import { expect, fn, userEvent, waitFor, within } from 'storybook/test'
import { DiagramView } from './DiagramView'
import { retrySpec } from './diagram.fixtures'

const meta: Meta<typeof DiagramView> = {
  title: 'Chat/DiagramView',
  component: DiagramView,
  args: { spec: retrySpec },
  parameters: { layout: 'padded', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta

type Story = StoryObj<typeof DiagramView>

export const Default: Story = {}

export const Dark: Story = { globals: { theme: 'dark' } }

// Real mermaid, real SVG: the click lands on the rendered node and the panel follows.
export const ClickNode: Story = {
  args: { onExplain: fn() },
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'], play: true } },
  play: async ({ canvasElement, args }) => {
    const c = within(canvasElement)
    await userEvent.click(await c.findByRole('button', { name: 'Node: Retry worker' }))
    await expect(await c.findByText(/never send the same delivery/)).toBeTruthy()
    await userEvent.click(await c.findByRole('button', { name: 'Explain more' }))
    await expect(args.onExplain).toHaveBeenCalledWith({ kind: 'node', id: 'worker' })
  },
}

export const ClickEdge: Story = {
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'], play: true } },
  play: async ({ canvasElement }) => {
    const c = within(canvasElement)
    await userEvent.click(await c.findByRole('button', { name: 'Connection: deliver() to retry_queue' }))
    await waitFor(() => expect(c.getByText(/next_at/)).toBeTruthy())
  },
}

export const ClickLayer: Story = {
  play: async ({ canvasElement }) => {
    const c = within(canvasElement)
    await userEvent.click(await c.findByRole('button', { name: 'Layer: Storage' }))
    await waitFor(() => expect(c.getByText(/Postgres tables/)).toBeTruthy())
  },
}

export const MobileViewport390: Story = {
  parameters: { layout: 'fullscreen' },
  decorators: [Story => (
    <div className="w-[390px] overflow-hidden border border-gray-300 bg-gray-50 p-3 dark:border-gray-600 dark:bg-gray-900">
      <Story />
    </div>
  )],
}
