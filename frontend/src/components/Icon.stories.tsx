import type { Meta, StoryObj } from '@storybook/react-vite'
import { Icon, type IconName } from './Icon'

const NAMES: IconName[] = [
  'close', 'check', 'send', 'edit', 'menu', 'help', 'warning',
  'mail', 'archive', 'music', 'folder', 'memory', 'extension', 'chat',
  'draw', 'monitoring', 'lightbulb',
]

const meta: Meta<typeof Icon> = {
  title: 'Chat/Icon',
  component: Icon,
  parameters: { layout: 'centered' },
}
export default meta

type Story = StoryObj<typeof Icon>

export const Single: Story = {
  args: { name: 'check', className: 'w-6 h-6' },
}

export const AllIcons: Story = {
  render: () => (
    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 16 }}>
      {NAMES.map(name => (
        <div key={name} style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 4 }}>
          <Icon name={name} className="w-6 h-6" />
          <span style={{ fontSize: 10 }}>{name}</span>
        </div>
      ))}
    </div>
  ),
}
