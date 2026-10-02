import type { Meta, StoryObj } from '@storybook/react-vite'
import { RiskTable } from './RiskTable'
import { retrySurface } from './diagram.fixtures'

const rows = (retrySurface.components.find(c => c.id === 'risk')?.rows ?? []) as React.ComponentProps<typeof RiskTable>['rows']

const meta: Meta<typeof RiskTable> = {
  title: 'Chat/RiskTable',
  component: RiskTable,
  args: { rows, basis: 'diff-only' },
  parameters: { layout: 'padded', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta

type Story = StoryObj<typeof RiskTable>

export const DiffOnly: Story = {}

export const RepoRead: Story = { args: { basis: 'repo' } }

export const Dark: Story = { globals: { theme: 'dark' } }
