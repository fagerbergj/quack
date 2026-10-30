import type { Meta, StoryObj } from '@storybook/react-vite'
import { Button } from './button'
import { Icon } from '../Icon'

const meta: Meta<typeof Button> = {
  title: 'UI/Button',
  component: Button,
  parameters: { layout: 'padded', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta

type Story = StoryObj<typeof Button>

const VARIANTS = ['default', 'secondary', 'outline', 'ghost', 'link', 'destructive'] as const

// Every variant, enabled and disabled; keyboard-tab through to see the focus ring.
export const Variants: Story = {
  render: () => (
    <div className="space-y-3">
      {[false, true].map(disabled => (
        <div key={String(disabled)} className="flex flex-wrap items-center gap-2">
          {VARIANTS.map(v => <Button key={v} variant={v} disabled={disabled}>{v}</Button>)}
        </div>
      ))}
    </div>
  ),
}

export const Sizes: Story = {
  render: () => (
    <div className="flex flex-wrap items-center gap-2">
      <Button>Default</Button>
      <Button size="sm" variant="outline">Small</Button>
      <Button size="icon" variant="ghost" aria-label="Refresh"><Icon name="refresh" className="size-4" /></Button>
      <Button size="inline" variant="link">Inline link-style</Button>
    </div>
  ),
}

// asChild styles the child element, so a real link keeps its href and semantics.
export const AsLink: Story = {
  render: () => <Button asChild variant="outline"><a href="#docs">Open docs</a></Button>,
}
