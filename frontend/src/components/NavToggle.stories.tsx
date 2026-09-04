import { useState } from 'react'
import type { Meta, StoryObj } from '@storybook/react-vite'
import { NavToggle } from './NavToggle'

const meta: Meta<typeof NavToggle> = {
  title: 'Chat/NavToggle',
  component: NavToggle,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof NavToggle>

export const Closed: Story = {
  args: { open: false, onToggle: () => {} },
}

export const Open: Story = {
  args: { open: true, onToggle: () => {} },
}

export const Interactive: Story = {
  render: () => {
    const [open, setOpen] = useState(false)
    return <NavToggle open={open} onToggle={() => setOpen(o => !o)} />
  },
}

export const Dark: Story = {
  ...Open,
  globals: { theme: 'dark' },
}
